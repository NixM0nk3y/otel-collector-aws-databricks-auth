package databricksauthextension

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"golang.org/x/sync/singleflight"
)

const (
	oidcTokenEndpoint      = "/oidc/v1/token"                                    // #nosec G101 -- URL path, not a credential
	grantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"   // #nosec G101 -- OAuth 2.0 grant type URI (RFC 8693)
	tokenTypeJWT           = "urn:ietf:params:oauth:token-type:jwt"              // #nosec G101 -- OAuth 2.0 token type URI (RFC 8693)
	defaultTokenTTL        = 1 * time.Hour
)

// AWSTokenProvider abstracts AWS identity token acquisition — mockable in tests.
type AWSTokenProvider interface {
	GetWebIdentityToken(ctx context.Context) (string, error)
}

// STSTokenProvider is the ECS/EC2 concrete implementation using aws-sdk-go-v2.
type STSTokenProvider struct {
	stsClient *sts.Client

	mu          sync.RWMutex
	cachedToken string
	tokenExpiry time.Time
}

// NewSTSTokenProvider creates an STSTokenProvider by loading the default AWS config.
func NewSTSTokenProvider(ctx context.Context) (*STSTokenProvider, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}
	return &STSTokenProvider{
		stsClient: sts.NewFromConfig(cfg),
	}, nil
}

// GetWebIdentityToken returns an AWS OIDC web identity token, caching it until near expiry.
func (p *STSTokenProvider) GetWebIdentityToken(ctx context.Context) (string, error) {
	const expiryBuffer = 30 * time.Second

	p.mu.RLock()
	if p.cachedToken != "" && time.Now().Before(p.tokenExpiry.Add(-expiryBuffer)) {
		token := p.cachedToken
		p.mu.RUnlock()
		return token, nil
	}
	p.mu.RUnlock()

	audience := "AwsTokenExchange"
	signingAlg := "RS256"
	output, err := p.stsClient.GetWebIdentityToken(ctx, &sts.GetWebIdentityTokenInput{
		Audience:         []string{audience},
		SigningAlgorithm: &signingAlg,
	})
	if err != nil {
		return "", fmt.Errorf("failed to get web identity token from STS: %w", err)
	}
	if output.WebIdentityToken == nil || *output.WebIdentityToken == "" {
		return "", fmt.Errorf("STS returned empty token")
	}

	p.mu.Lock()
	p.cachedToken = *output.WebIdentityToken
	if output.Expiration != nil {
		p.tokenExpiry = *output.Expiration
	} else {
		p.tokenExpiry = time.Now().Add(5 * time.Minute)
	}
	p.mu.Unlock()

	return *output.WebIdentityToken, nil
}

// tokenExchangeResponse is the success response from the OIDC token endpoint.
type tokenExchangeResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// tokenExchangeErrorResponse is the error response from the OIDC token endpoint.
type tokenExchangeErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// tokenCache holds a Databricks access token with lazy refresh and singleflight coalescing.
type tokenCache struct {
	workspaceURL string
	spClientID   string
	expiryBuffer time.Duration
	awsProvider  AWSTokenProvider
	httpClient   *http.Client

	mu          sync.RWMutex
	cachedToken string
	tokenExpiry time.Time
	sfGroup     singleflight.Group
}

// GetToken returns a valid Databricks access token, refreshing it transparently when near expiry.
func (c *tokenCache) GetToken(ctx context.Context) (string, error) {
	// Fast path: check cache under read lock.
	c.mu.RLock()
	if c.cachedToken != "" && time.Now().Before(c.tokenExpiry.Add(-c.expiryBuffer)) {
		token := c.cachedToken
		c.mu.RUnlock()
		return token, nil
	}
	c.mu.RUnlock()

	// Slow path: use singleflight to coalesce concurrent refreshes.
	result, err, _ := c.sfGroup.Do("token", func() (interface{}, error) {
		// Double-check inside singleflight in case another goroutine just refreshed.
		c.mu.RLock()
		if c.cachedToken != "" && time.Now().Before(c.tokenExpiry.Add(-c.expiryBuffer)) {
			token := c.cachedToken
			c.mu.RUnlock()
			return token, nil
		}
		c.mu.RUnlock()

		token, expiresIn, err := c.exchangeToken(ctx)
		if err != nil {
			return "", err
		}

		expiry := time.Now().Add(time.Duration(expiresIn) * time.Second)
		c.mu.Lock()
		c.cachedToken = token
		c.tokenExpiry = expiry
		c.mu.Unlock()

		return token, nil
	})
	if err != nil {
		return "", err
	}
	return result.(string), nil
}

// exchangeToken performs the OAuth 2.0 Token Exchange (RFC 8693) against the Databricks OIDC endpoint.
func (c *tokenCache) exchangeToken(ctx context.Context) (string, int, error) {
	awsToken, err := c.awsProvider.GetWebIdentityToken(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("failed to get AWS token: %w", err)
	}

	tokenURL := c.workspaceURL + oidcTokenEndpoint
	formData := url.Values{}
	formData.Set("grant_type", grantTypeTokenExchange)
	formData.Set("subject_token", awsToken)
	formData.Set("subject_token_type", tokenTypeJWT)
	formData.Set("client_id", c.spClientID)
	formData.Set("scope", "all-apis")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("failed to create token exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("token exchange request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("failed to read token exchange response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp tokenExchangeErrorResponse
		if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
			return "", 0, fmt.Errorf("token exchange failed (%s): %s", errResp.Error, errResp.ErrorDescription)
		}
		return "", 0, fmt.Errorf("token exchange failed with status %d", resp.StatusCode)
	}

	var tokenResp tokenExchangeResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", 0, fmt.Errorf("failed to parse token exchange response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", 0, fmt.Errorf("token exchange response missing access_token")
	}

	expiresIn := tokenResp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = int(defaultTokenTTL.Seconds())
	}

	return tokenResp.AccessToken, expiresIn, nil
}
