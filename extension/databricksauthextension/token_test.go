package databricksauthextension

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockAWSTokenProvider is a mock for AWSTokenProvider, usable in tests.
type mockAWSTokenProvider struct {
	token     string
	err       error
	callCount atomic.Int32
}

func (m *mockAWSTokenProvider) GetWebIdentityToken(_ context.Context) (string, error) {
	m.callCount.Add(1)
	return m.token, m.err
}

// createMockOIDCServer spins up a test OIDC endpoint that returns a canned token response.
func createMockOIDCServer(t *testing.T, accessToken string, expiresIn int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != oidcTokenEndpoint {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenExchangeResponse{
			AccessToken: accessToken,
			TokenType:   "Bearer",
			ExpiresIn:   expiresIn,
		})
	}))
}

// createMockOIDCServerWithCounter is like createMockOIDCServer but increments counter on each hit.
func createMockOIDCServerWithCounter(t *testing.T, accessToken string, expiresIn int, counter *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.Add(1)
		if r.URL.Path != oidcTokenEndpoint {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenExchangeResponse{
			AccessToken: accessToken,
			TokenType:   "Bearer",
			ExpiresIn:   expiresIn,
		})
	}))
}

func newTestTokenCache(workspaceURL string, provider AWSTokenProvider) *tokenCache {
	return &tokenCache{
		workspaceURL: workspaceURL,
		spClientID:   "test-client-id",
		expiryBuffer: 5 * time.Minute,
		awsProvider:  provider,
		httpClient:   &http.Client{Timeout: 5 * time.Second},
	}
}

// TestTokenCache_CacheHit verifies the server is called only once and the second call uses the cache.
func TestTokenCache_CacheHit(t *testing.T) {
	server := createMockOIDCServer(t, "test-token-123", 3600)
	defer server.Close()

	mock := &mockAWSTokenProvider{token: "aws-token"}
	cache := newTestTokenCache(server.URL, mock)

	ctx := context.Background()

	tok1, err := cache.GetToken(ctx)
	if err != nil {
		t.Fatalf("first GetToken: %v", err)
	}
	if tok1 != "test-token-123" {
		t.Fatalf("expected test-token-123, got %s", tok1)
	}

	tok2, err := cache.GetToken(ctx)
	if err != nil {
		t.Fatalf("second GetToken: %v", err)
	}
	if tok2 != tok1 {
		t.Errorf("expected same token from cache, got %s", tok2)
	}

	// AWS provider should be called exactly once.
	if n := mock.callCount.Load(); n != 1 {
		t.Errorf("expected 1 AWS call, got %d", n)
	}
}

// TestTokenCache_CacheExpiry verifies that a token within the expiry buffer is refreshed.
func TestTokenCache_CacheExpiry(t *testing.T) {
	var counter atomic.Int32
	server := createMockOIDCServerWithCounter(t, "test-token", 3600, &counter)
	defer server.Close()

	mock := &mockAWSTokenProvider{token: "aws-token"}
	cache := newTestTokenCache(server.URL, mock)

	ctx := context.Background()

	_, err := cache.GetToken(ctx)
	if err != nil {
		t.Fatalf("first GetToken: %v", err)
	}
	if counter.Load() != 1 {
		t.Fatalf("expected 1 server request, got %d", counter.Load())
	}

	// Move expiry inside the buffer window (< 5 min away).
	cache.mu.Lock()
	cache.tokenExpiry = time.Now().Add(4 * time.Minute)
	cache.mu.Unlock()

	_, err = cache.GetToken(ctx)
	if err != nil {
		t.Fatalf("second GetToken: %v", err)
	}
	if counter.Load() != 2 {
		t.Errorf("expected 2 server requests after cache expiry, got %d", counter.Load())
	}
}

// TestTokenCache_ExpiredTokenNotReturned verifies that a fully-expired cached token is never served.
func TestTokenCache_ExpiredTokenNotReturned(t *testing.T) {
	var counter atomic.Int32
	server := createMockOIDCServerWithCounter(t, "fresh-token", 3600, &counter)
	defer server.Close()

	mock := &mockAWSTokenProvider{token: "aws-token"}
	cache := newTestTokenCache(server.URL, mock)

	// Inject an already-expired token directly.
	cache.mu.Lock()
	cache.cachedToken = "expired-DO-NOT-USE"
	cache.tokenExpiry = time.Now().Add(-1 * time.Hour)
	cache.mu.Unlock()

	tok, err := cache.GetToken(context.Background())
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if tok == "expired-DO-NOT-USE" {
		t.Fatal("CRITICAL: expired token was returned from cache")
	}
	if tok != "fresh-token" {
		t.Errorf("expected fresh-token, got %s", tok)
	}
	if counter.Load() != 1 {
		t.Errorf("expected 1 server request, got %d", counter.Load())
	}
}

// TestTokenCache_ExpiresInZero verifies that ExpiresIn == 0 falls back to the default TTL.
func TestTokenCache_ExpiresInZero(t *testing.T) {
	server := createMockOIDCServer(t, "tok", 0 /* ExpiresIn */)
	defer server.Close()

	mock := &mockAWSTokenProvider{token: "aws-token"}
	cache := newTestTokenCache(server.URL, mock)

	before := time.Now()
	_, err := cache.GetToken(context.Background())
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}

	cache.mu.RLock()
	expiry := cache.tokenExpiry
	cache.mu.RUnlock()

	expectedMin := before.Add(defaultTokenTTL - time.Second)
	if expiry.Before(expectedMin) {
		t.Errorf("expected expiry ~%v away, got %v", defaultTokenTTL, time.Until(expiry))
	}
}

// TestTokenCache_ConcurrentAccess verifies singleflight: 100 goroutines → 1 server request.
func TestTokenCache_ConcurrentAccess(t *testing.T) {
	var counter atomic.Int32
	server := createMockOIDCServerWithCounter(t, "concurrent-token", 3600, &counter)
	defer server.Close()

	mock := &mockAWSTokenProvider{token: "aws-token"}
	cache := newTestTokenCache(server.URL, mock)

	ctx := context.Background()
	const n = 100

	var wg sync.WaitGroup
	tokens := make([]string, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			tokens[idx], errs[idx] = cache.GetToken(ctx)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d error: %v", i, err)
		}
	}
	for i, tok := range tokens {
		if tok != "concurrent-token" {
			t.Errorf("goroutine %d got wrong token: %s", i, tok)
		}
	}

	if req := counter.Load(); req != 1 {
		t.Errorf("singleflight not working: expected 1 server request, got %d", req)
	}
}

// TestTokenCache_AWSProviderError verifies error propagation from the AWS provider.
func TestTokenCache_AWSProviderError(t *testing.T) {
	server := createMockOIDCServer(t, "tok", 3600)
	defer server.Close()

	mock := &mockAWSTokenProvider{err: fmt.Errorf("aws cred error")}
	cache := newTestTokenCache(server.URL, mock)

	_, err := cache.GetToken(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestTokenCache_OIDCReturns401 verifies that a 401 from the OIDC endpoint surfaces as an error.
func TestTokenCache_OIDCReturns401(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(tokenExchangeErrorResponse{
			Error:            "invalid_client",
			ErrorDescription: "bad credentials",
		})
	}))
	defer server.Close()

	mock := &mockAWSTokenProvider{token: "aws-token"}
	cache := newTestTokenCache(server.URL, mock)

	_, err := cache.GetToken(context.Background())
	if err == nil {
		t.Fatal("expected error for 401, got nil")
	}
}

// TestTokenCache_OIDCReturns429 verifies that a 429 from the OIDC endpoint surfaces as an error.
func TestTokenCache_OIDCReturns429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	mock := &mockAWSTokenProvider{token: "aws-token"}
	cache := newTestTokenCache(server.URL, mock)

	_, err := cache.GetToken(context.Background())
	if err == nil {
		t.Fatal("expected error for 429, got nil")
	}
}

// TestTokenCache_ServerUnreachable verifies that a connection error from the OIDC endpoint surfaces.
func TestTokenCache_ServerUnreachable(t *testing.T) {
	// Start and immediately close a server so its port is refused.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := server.URL
	server.Close()

	mock := &mockAWSTokenProvider{token: "aws-token"}
	cache := newTestTokenCache(url, mock)

	_, err := cache.GetToken(context.Background())
	if err == nil {
		t.Fatal("expected error for unreachable server, got nil")
	}
}

// TestTokenCache_MalformedJSONOnSuccess verifies that a 200 with non-JSON body is an error.
func TestTokenCache_MalformedJSONOnSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not-json{{{"))
	}))
	defer server.Close()

	mock := &mockAWSTokenProvider{token: "aws-token"}
	cache := newTestTokenCache(server.URL, mock)

	_, err := cache.GetToken(context.Background())
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

// TestTokenCache_MissingAccessToken verifies that a 200 with no access_token is an error.
func TestTokenCache_MissingAccessToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Deliberately omit access_token.
		json.NewEncoder(w).Encode(map[string]any{"token_type": "Bearer"})
	}))
	defer server.Close()

	mock := &mockAWSTokenProvider{token: "aws-token"}
	cache := newTestTokenCache(server.URL, mock)

	_, err := cache.GetToken(context.Background())
	if err == nil {
		t.Fatal("expected error for missing access_token, got nil")
	}
}
