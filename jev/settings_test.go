package jev_test

import (
	"testing"
	"time"

	"github.com/kpenfound/agentcore/jev"
	"github.com/kpenfound/agentcore/jev/systemone"
)

func TestDefaultsReachTypeSafe(t *testing.T) {
	if jev.DefaultURL != "https://api.typesafe.ai" {
		t.Fatalf("default URL = %q", jev.DefaultURL)
	}
	if jev.DefaultURL != systemone.DefaultBaseURL {
		t.Fatalf("judge default URL = %q, client default = %q", jev.DefaultURL, systemone.DefaultBaseURL)
	}
	if jev.DefaultModel != "jev-latest" || jev.DefaultAPIKeyEnv != "TYPESAFE_API_KEY" || jev.DefaultTimeout != 10*time.Second {
		t.Fatalf("default model = %q, key variable = %q, timeout = %v", jev.DefaultModel, jev.DefaultAPIKeyEnv, jev.DefaultTimeout)
	}
	want := jev.Settings{
		URL:       "https://api.typesafe.ai",
		Model:     "jev-latest",
		APIKeyEnv: "TYPESAFE_API_KEY",
		Timeout:   10 * time.Second,
	}
	settings := jev.DefaultSettings()
	if settings != want {
		t.Fatalf("default settings = %+v, want %+v", settings, want)
	}
	settings.Enabled = true
	if err := settings.Validate(); err != nil {
		t.Fatalf("enabled default settings: %v", err)
	}
}
