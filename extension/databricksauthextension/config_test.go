package databricksauthextension

import (
	"testing"
	"time"

	"go.opentelemetry.io/collector/config/configopaque"
)

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name:    "empty config",
			cfg:     Config{},
			wantErr: true,
		},
		{
			name:    "static token only",
			cfg:     Config{Token: configopaque.String("my-token")},
			wantErr: false,
		},
		{
			name:    "sp_client_id and workspace_url",
			cfg:     Config{SPClientID: "client-id", WorkspaceURL: "https://adb-123.cloud.databricks.com"},
			wantErr: false,
		},
		{
			name:    "both token and sp_client_id",
			cfg:     Config{Token: "tok", SPClientID: "client-id", WorkspaceURL: "https://adb-123.cloud.databricks.com"},
			wantErr: true,
		},
		{
			name:    "sp_client_id without workspace_url",
			cfg:     Config{SPClientID: "client-id"},
			wantErr: true,
		},
		{
			name:    "sp_client_id with empty expiry_buffer uses default",
			cfg:     Config{SPClientID: "client-id", WorkspaceURL: "https://adb-123.cloud.databricks.com"},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestConfig_expiryBufferOrDefault(t *testing.T) {
	t.Run("returns default when zero", func(t *testing.T) {
		cfg := Config{}
		if got := cfg.expiryBufferOrDefault(); got != 5*time.Minute {
			t.Errorf("expected 5m, got %v", got)
		}
	})

	t.Run("returns configured value when set", func(t *testing.T) {
		cfg := Config{ExpiryBuffer: 2 * time.Minute}
		if got := cfg.expiryBufferOrDefault(); got != 2*time.Minute {
			t.Errorf("expected 2m, got %v", got)
		}
	})
}
