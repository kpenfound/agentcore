package jev_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/kpenfound/agentcore/jev"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBuiltInProviderUsesConnectionSettings(t *testing.T) {
	for _, tc := range []struct {
		name, base, model, keyEnv, path string
	}{
		{"TypeSafe defaults", "https://api.typesafe.ai", "jev-latest", "TYPESAFE_API_KEY", "/v1/systemone"},
		{"OpenRouter", "https://openrouter.ai/api", "~typesafe/jev-latest", "OPENROUTER_API_KEY", "/api/v1/systemone"},
		{"custom endpoint", "", "custom-model", "TEST_CUSTOM_JEV_KEY", "/custom/v1/systemone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := jev.DefaultSettings()
			settings.Enabled = true
			if tc.name != "TypeSafe defaults" {
				settings.URL, settings.Model, settings.APIKeyEnv = tc.base, tc.model, tc.keyEnv
			}
			t.Setenv("TYPESAFE_API_KEY", "unused-typesafe-credential")
			t.Setenv("OPENROUTER_API_KEY", "unused-openrouter-credential")
			t.Setenv("TEST_CUSTOM_JEV_KEY", "unused-custom-credential")
			t.Setenv(tc.keyEnv, "test-credential")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != tc.path {
					t.Errorf("request = %s %s, want POST %s", r.Method, r.URL.Path, tc.path)
				}
				if r.Header.Get("Authorization") != "Bearer test-credential" {
					t.Error("bearer credential does not match the configured environment variable")
				}
				var body struct {
					Model string `json:"model"`
					State string `json:"state"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				} else if body.Model != tc.model || body.State != "Done." {
					t.Errorf("request body = %+v", body)
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(answer().Response); err != nil {
					t.Errorf("encode response: %v", err)
				}
			}))
			defer server.Close()
			if tc.base == "" {
				settings.URL = server.URL + "/custom/"
			} else {
				// Route the literal service URL to a local server while keeping
				// the Judge's nil Provider and the real systemone.Client.
				local, err := url.Parse(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				transport := http.DefaultTransport
				t.Cleanup(func() { http.DefaultTransport = transport })
				http.DefaultTransport = roundTrip(func(r *http.Request) (*http.Response, error) {
					if got, want := r.URL.String(), tc.base+"/v1/systemone"; got != want {
						t.Errorf("request URL = %q, want %q", got, want)
					}
					forward := r.Clone(r.Context())
					forward.URL.Scheme, forward.URL.Host = local.Scheme, local.Host
					return transport.RoundTrip(forward)
				})
			}
			if err := settings.Validate(); err != nil {
				t.Fatalf("connection settings: %v", err)
			}
			j := &jev.Judge{Config: func() jev.Settings { return settings }}
			d := j.Evaluate(context.Background(), request("Done."))
			if d.Outcome != jev.Accepted || d.Requests != 1 || d.Response == nil || d.Response.Answers["class"].Choice != "claims_done" {
				t.Fatalf("decision = %+v", d)
			}
		})
	}
}
