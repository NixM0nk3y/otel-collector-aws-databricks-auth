package databricksauthextension

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.uber.org/zap"
)

type databricksAuthExtension struct {
	cfg    *Config
	logger *zap.Logger
	cache  *tokenCache // nil in static mode
}

// newAWSProvider is the constructor used by Start. Replaced in tests to inject failures.
var newAWSProvider = func(ctx context.Context) (AWSTokenProvider, error) {
	return NewSTSTokenProvider(ctx)
}

func (e *databricksAuthExtension) Start(ctx context.Context, _ component.Host) error {
	if e.cfg.SPClientID == "" {
		return nil // static mode
	}
	awsProvider, err := newAWSProvider(ctx)
	if err != nil {
		return fmt.Errorf("failed to init AWS provider: %w", err)
	}
	e.cache = &tokenCache{
		workspaceURL: e.cfg.WorkspaceURL,
		spClientID:   e.cfg.SPClientID,
		expiryBuffer: e.cfg.expiryBufferOrDefault(),
		awsProvider:  awsProvider,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
	}
	return nil
}

func (e *databricksAuthExtension) Shutdown(_ context.Context) error { return nil }

// RoundTripper implements extensionauth.HTTPClient.
func (e *databricksAuthExtension) RoundTripper(base http.RoundTripper) (http.RoundTripper, error) {
	return &bearerRoundTripper{ext: e, base: base}, nil
}

type bearerRoundTripper struct {
	ext  *databricksAuthExtension
	base http.RoundTripper
}

func (rt *bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	var token string
	if rt.ext.cache != nil {
		var err error
		token, err = rt.ext.cache.GetToken(req.Context())
		if err != nil {
			return nil, err
		}
	} else {
		token = string(rt.ext.cfg.Token)
	}
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+token)
	return rt.base.RoundTrip(r)
}
