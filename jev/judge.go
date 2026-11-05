// Package jev asks Jev, TypeSafe's System One model, typed questions through
// a Provider, by default TypeSafe's own API. OpenRouter is an explicit
// alternative: set Settings.URL to "https://openrouter.ai/api", Model to
// "~typesafe/jev-latest" and APIKeyEnv to "OPENROUTER_API_KEY". A Judge adds
// what a service needs around the API client in jev/systemone: one switch,
// the API key from the environment, a retry of transient failures, a
// cool-down that keeps an outage from becoming a retry storm, and a status.
// Every failure is a fallback with a reason, never an error to handle. A
// request waits on the network, so nothing on a scheduling path may make one.
package jev

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/kpenfound/agentcore/jev/systemone"
)

// maxRequests bounds the requests of one evaluation; only a timeout or an
// unavailable provider is retried.
const maxRequests = 2

// Outcome is whether an evaluation's answers can be used.
type Outcome string

const (
	Accepted Outcome = "accepted"
	Fallback Outcome = "fallback"
)

// Decision is an evaluation's result. Use the answers only when Outcome is
// Accepted; on a fallback, take the path the workflow takes without Jev.
type Decision struct {
	Outcome Outcome
	// Reason and Detail say why a fallback fell back.
	Reason Reason
	Detail string
	// Response holds the validated answers, the model that gave them and
	// what the request consumed. It is set when Outcome is Accepted.
	Response *systemone.Response
	// Requests counts the requests sent, a retry included, and Duration
	// how long they took.
	Requests int
	Duration time.Duration
}

// Judge asks Jev on behalf of every caller of a service. Share one Judge, so
// an outage cools every caller down together. A nil Judge is off.
type Judge struct {
	// Config returns the current settings.
	Config func() Settings
	// Provider returns the provider for settings and a key. Nil uses a
	// systemone.Client for the settings' URL and model.
	Provider func(settings Settings, key string) Provider
	// Getenv reads the API key. Nil uses os.Getenv.
	Getenv func(string) string
	// Now supplies the time. Nil uses time.Now.
	Now func() time.Time

	mu       sync.Mutex
	settings Settings
	cool     *coolDown
}

func (j *Judge) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

func (j *Judge) key(settings Settings) string {
	if j.Getenv != nil {
		return j.Getenv(settings.APIKeyEnv)
	}
	return os.Getenv(settings.APIKeyEnv)
}

// coolDown returns the cool-down of the current settings. Changed settings
// reach a provider afresh.
func (j *Judge) coolDown(settings Settings) *coolDown {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.cool == nil || j.settings != settings {
		j.settings, j.cool = settings, &coolDown{}
	}
	return j.cool
}

func (j *Judge) provider(settings Settings, key string) Provider {
	if j.Provider != nil {
		return j.Provider(settings, key)
	}
	return systemone.Client{BaseURL: settings.URL, Model: settings.Model, APIKey: key}
}

// Evaluate asks r's questions. While the Judge is off it returns a disabled
// fallback without sending anything. It sends nothing while the provider
// cools down, retries a timeout or an unavailable provider once, and waits
// for at most two requests of the configured timeout. When ctx ends it
// returns a cancelled fallback.
func (j *Judge) Evaluate(ctx context.Context, r systemone.Request) Decision {
	if j == nil {
		return Decision{Outcome: Fallback, Reason: ReasonDisabled}
	}
	settings := j.Config()
	if !settings.Enabled {
		return Decision{Outcome: Fallback, Reason: ReasonDisabled}
	}
	if err := settings.Validate(); err != nil {
		return Decision{Outcome: Fallback, Reason: ReasonUnconfigured, Detail: err.Error()}
	}
	if err := r.Validate(); err != nil {
		return Decision{Outcome: Fallback, Reason: ReasonRejected, Detail: err.Error()}
	}
	key := j.key(settings)
	if key == "" {
		return Decision{Outcome: Fallback, Reason: ReasonUnconfigured, Detail: settings.APIKeyEnv + " is not set"}
	}
	cool := j.coolDown(settings)
	if cooling, until := cool.cooling(j.now()); cooling {
		return Decision{Outcome: Fallback, Reason: ReasonCoolingDown, Detail: "provider is cooling down until " + until.UTC().Format(time.RFC3339)}
	}

	provider := j.provider(settings, key)
	begin := j.now()
	var d Decision
	for d.Requests < maxRequests {
		d.Requests++
		call, cancel := context.WithTimeout(ctx, settings.Timeout)
		resp, err := provider.Evaluate(call, r)
		cancel()
		if err == nil {
			cool.observe(j.now(), nil)
			d.Outcome, d.Response, d.Duration = Accepted, &resp, j.now().Sub(begin)
			return d
		}
		f := failed(err)
		d.Outcome, d.Reason, d.Detail = Fallback, f.reason, f.detail
		if ctx.Err() != nil {
			d.Reason, d.Detail = ReasonCancelled, "context ended during the request"
			break
		}
		cool.observe(j.now(), f)
		if paused, _ := cool.cooling(j.now()); paused || !f.reason.transient() {
			break
		}
	}
	d.Duration = j.now().Sub(begin)
	return d
}

// Mode is the Judge's state as Status reports it.
type Mode string

const (
	ModeDisabled Mode = "disabled"
	// ModeUnconfigured is an enabled Judge without its API key or with
	// invalid settings; every evaluation falls back.
	ModeUnconfigured Mode = "unconfigured"
	ModeReady        Mode = "ready"
	// ModeDegraded is an enabled Judge whose latest request failed or that
	// is cooling down; evaluations fall back while it cools.
	ModeDegraded Mode = "degraded"
)

// Status reports the Judge's mode, the configured model and the latest
// failure since the last success.
type Status struct {
	Mode         Mode       `json:"mode"`
	Model        string     `json:"model,omitempty"`
	Reason       Reason     `json:"reason,omitempty"`
	Detail       string     `json:"detail,omitempty"`
	CoolingUntil *time.Time `json:"cooling_until,omitempty"`
}

// Status reports the Judge under the current settings.
func (j *Judge) Status() Status {
	if j == nil {
		return Status{Mode: ModeDisabled}
	}
	settings := j.Config()
	if !settings.Enabled {
		return Status{Mode: ModeDisabled}
	}
	out := Status{Mode: ModeReady, Model: settings.Model}
	if err := settings.Validate(); err != nil {
		out.Mode, out.Reason, out.Detail = ModeUnconfigured, ReasonUnconfigured, err.Error()
		return out
	}
	if j.key(settings) == "" {
		out.Mode, out.Reason, out.Detail = ModeUnconfigured, ReasonUnconfigured, settings.APIKeyEnv+" is not set"
		return out
	}
	cool := j.coolDown(settings)
	if f := cool.lastFailure(); f != nil {
		out.Mode, out.Reason, out.Detail = ModeDegraded, f.reason, f.detail
	}
	if cooling, until := cool.cooling(j.now()); cooling {
		out.Mode, out.CoolingUntil = ModeDegraded, &until
	}
	return out
}
