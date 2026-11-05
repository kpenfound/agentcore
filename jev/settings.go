package jev

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/kpenfound/agentcore/jev/systemone"
)

// The defaults reach Jev through TypeSafe's own API. OpenRouter is an explicit
// alternative: URL "https://openrouter.ai/api", Model "~typesafe/jev-latest",
// APIKeyEnv "OPENROUTER_API_KEY".
const (
	DefaultURL       = systemone.DefaultBaseURL
	DefaultModel     = "jev-latest"
	DefaultAPIKeyEnv = "TYPESAFE_API_KEY"
	DefaultTimeout   = 10 * time.Second
)

// MaxTimeout bounds one request, so an evaluation never holds its caller for
// long.
const MaxTimeout = time.Minute

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Settings say whether a Judge asks Jev and how it reaches Jev. Enabled is the
// one switch for every evaluation a Judge serves. The API key is the name of
// an environment variable; its value never becomes a setting or a record.
// DefaultSettings selects TypeSafe's own API. To select OpenRouter explicitly,
// set URL to "https://openrouter.ai/api", Model to "~typesafe/jev-latest" and
// APIKeyEnv to "OPENROUTER_API_KEY".
type Settings struct {
	Enabled bool
	// URL is the base of a TypeSafe-compatible API; requests go to
	// <URL>/v1/systemone.
	URL       string
	Model     string
	APIKeyEnv string
	// Timeout bounds one request.
	Timeout time.Duration
}

// DefaultSettings returns disabled settings for TypeSafe's own API.
func DefaultSettings() Settings {
	return Settings{URL: DefaultURL, Model: DefaultModel, APIKeyEnv: DefaultAPIKeyEnv, Timeout: DefaultTimeout}
}

// Validate checks every setting, whether or not Enabled is set, so turning the
// switch on never reveals a broken setting.
func (s Settings) Validate() error {
	u, err := url.Parse(s.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("jev URL must be an HTTP(S) URL without credentials, query or fragment")
	}
	if strings.TrimSpace(s.Model) == "" || strings.ContainsAny(s.Model, " \t\r\n") || len(s.Model) > 128 {
		return errors.New("jev model must be named in up to 128 characters without whitespace")
	}
	if !environmentName.MatchString(s.APIKeyEnv) {
		return errors.New("jev API key setting must name an environment variable")
	}
	if s.Timeout <= 0 || s.Timeout > MaxTimeout {
		return errors.New("jev timeout must be positive and at most 1m")
	}
	return nil
}
