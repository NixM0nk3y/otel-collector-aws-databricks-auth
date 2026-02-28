package databricksauthextension

import (
	"errors"
	"time"

	"go.opentelemetry.io/collector/config/configopaque"
)

// Config holds the configuration for the Databricks authenticator extension.
type Config struct {
	// Static mode (local dev). Mutually exclusive with federation fields.
	Token configopaque.String `mapstructure:"token"`

	// Federation mode (AWS→Databricks).
	WorkspaceURL string        `mapstructure:"workspace_url"` // e.g. https://adb-xxx.cloud.databricks.com
	SPClientID   string        `mapstructure:"sp_client_id"`  // Databricks SP OAuth app client ID
	ExpiryBuffer time.Duration `mapstructure:"expiry_buffer"` // default: 5m
}

func (c *Config) Validate() error {
	hasStatic := c.Token != ""
	hasFed := c.SPClientID != ""
	switch {
	case !hasStatic && !hasFed:
		return errors.New("either token or sp_client_id must be configured")
	case hasStatic && hasFed:
		return errors.New("token and sp_client_id are mutually exclusive")
	case hasFed && c.WorkspaceURL == "":
		return errors.New("workspace_url is required when sp_client_id is set")
	}
	return nil
}

func (c *Config) expiryBufferOrDefault() time.Duration {
	if c.ExpiryBuffer > 0 {
		return c.ExpiryBuffer
	}
	return 5 * time.Minute
}
