package databricksauthextension

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configopaque"
	"go.opentelemetry.io/collector/extension"
	"go.uber.org/zap"
)

// fakeBackend records the Authorization header of incoming requests.
func fakeBackend(t *testing.T, gotAuth *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
}

func newExt(cfg *Config) *databricksAuthExtension {
	return &databricksAuthExtension{cfg: cfg, logger: zap.NewNop()}
}

// TestRoundTripper_StaticMode verifies that the static token is injected as Bearer auth.
func TestRoundTripper_StaticMode(t *testing.T) {
	var gotAuth string
	backend := fakeBackend(t, &gotAuth)
	defer backend.Close()

	ext := newExt(&Config{Token: configopaque.String("my-static-token")})
	rt, err := ext.RoundTripper(http.DefaultTransport)
	if err != nil {
		t.Fatalf("RoundTripper: %v", err)
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, backend.URL, nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	want := "Bearer my-static-token"
	if gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
}

// TestRoundTripper_FederationMode verifies the federation path injects the token from the cache.
func TestRoundTripper_FederationMode(t *testing.T) {
	var gotAuth string
	backend := fakeBackend(t, &gotAuth)
	defer backend.Close()

	ext := newExt(&Config{
		SPClientID:   "client-id",
		WorkspaceURL: "https://adb-123.azuredatabricks.net",
	})

	// Inject a pre-populated cache so no real AWS/OIDC calls are made.
	ext.cache = &tokenCache{
		workspaceURL: "https://adb-123.azuredatabricks.net",
		spClientID:   "client-id",
		expiryBuffer: 5 * time.Minute,
		awsProvider:  &mockAWSTokenProvider{token: "aws-tok"},
		httpClient:   &http.Client{},
		cachedToken:  "federated-token",
		tokenExpiry:  time.Now().Add(1 * time.Hour),
	}

	rt, err := ext.RoundTripper(http.DefaultTransport)
	if err != nil {
		t.Fatalf("RoundTripper: %v", err)
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, backend.URL, nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	want := "Bearer federated-token"
	if gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
}

// TestRoundTripper_PropagatesGetTokenError verifies that a GetToken failure bubbles up.
func TestRoundTripper_PropagatesGetTokenError(t *testing.T) {
	ext := newExt(&Config{
		SPClientID:   "client-id",
		WorkspaceURL: "https://adb-123.azuredatabricks.net",
	})

	// Use a provider that always fails so exchangeToken will error.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	ext.cache = &tokenCache{
		workspaceURL: server.URL,
		spClientID:   "client-id",
		expiryBuffer: 5 * time.Minute,
		awsProvider:  &mockAWSTokenProvider{err: fmt.Errorf("aws down")},
		httpClient:   &http.Client{Timeout: 5 * time.Second},
	}

	rt, err := ext.RoundTripper(http.DefaultTransport)
	if err != nil {
		t.Fatalf("RoundTripper: %v", err)
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	_, err = rt.RoundTrip(req)
	if err == nil {
		t.Fatal("expected error from GetToken, got nil")
	}
}

// TestStart_StaticMode verifies Start() is a no-op in static mode (no cache initialised).
func TestStart_StaticMode(t *testing.T) {
	ext := newExt(&Config{Token: configopaque.String("tok")})
	if err := ext.Start(context.Background(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if ext.cache != nil {
		t.Error("expected cache to be nil in static mode")
	}
}

// TestShutdown verifies Shutdown is always a no-op.
func TestShutdown(t *testing.T) {
	ext := newExt(&Config{Token: configopaque.String("tok")})
	if err := ext.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// TestStart_FederationMode verifies Start initialises the tokenCache when sp_client_id is set.
func TestStart_FederationMode(t *testing.T) {
	ext := newExt(&Config{
		SPClientID:   "client-id",
		WorkspaceURL: "https://adb-123.azuredatabricks.net",
	})
	if err := ext.Start(context.Background(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if ext.cache == nil {
		t.Error("expected tokenCache to be initialised in federation mode")
	}
}

// TestStart_FederationMode_AWSError verifies Start returns an error when the AWS provider fails.
func TestStart_FederationMode_AWSError(t *testing.T) {
	old := newAWSProvider
	newAWSProvider = func(_ context.Context) (AWSTokenProvider, error) {
		return nil, fmt.Errorf("no credentials")
	}
	defer func() { newAWSProvider = old }()

	ext := newExt(&Config{
		SPClientID:   "client-id",
		WorkspaceURL: "https://adb-123.azuredatabricks.net",
	})
	if err := ext.Start(context.Background(), nil); err == nil {
		t.Fatal("expected error when AWS provider fails, got nil")
	}
}

// TestCreateDefaultConfig verifies the factory creates a zero-value *Config.
func TestCreateDefaultConfig(t *testing.T) {
	cfg := createDefaultConfig()
	if cfg == nil {
		t.Fatal("createDefaultConfig returned nil")
	}
	if _, ok := cfg.(*Config); !ok {
		t.Fatalf("expected *Config, got %T", cfg)
	}
}

// TestCreateExtension verifies the factory wires up the extension correctly.
func TestCreateExtension(t *testing.T) {
	cfg := createDefaultConfig()
	set := extension.Settings{
		TelemetrySettings: component.TelemetrySettings{Logger: zap.NewNop()},
	}
	ext, err := createExtension(context.Background(), set, cfg)
	if err != nil {
		t.Fatalf("createExtension: %v", err)
	}
	if ext == nil {
		t.Fatal("createExtension returned nil")
	}
}

// TestNewFactory verifies the factory registers under the expected component type.
func TestNewFactory(t *testing.T) {
	f := NewFactory()
	if f.Type() != componentType {
		t.Errorf("factory type = %v, want %v", f.Type(), componentType)
	}
}

// TestRoundTripper_DoesNotMutateOriginalRequest verifies the original request is not modified.
func TestRoundTripper_DoesNotMutateOriginalRequest(t *testing.T) {
	var requestCount atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	ext := newExt(&Config{Token: configopaque.String("tok")})
	rt, _ := ext.RoundTripper(http.DefaultTransport)

	orig, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, backend.URL, nil)
	orig.Header.Set("X-Custom", "original")

	resp, err := rt.RoundTrip(orig)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	// The original request should not have the Authorization header added to it.
	if orig.Header.Get("Authorization") != "" {
		t.Error("original request was mutated with Authorization header")
	}
}
