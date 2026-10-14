package jev

import (
	"context"
	"errors"
	"time"

	"github.com/kpenfound/agentcore/jev/systemone"
)

// Provider answers System One requests. A Judge's default provider is a
// systemone.Client; tests use jevtest.
type Provider interface {
	Evaluate(context.Context, systemone.Request) (systemone.Response, error)
}

// Reason says why an evaluation fell back instead of using Jev's answers.
type Reason string

const (
	// ReasonDisabled is an evaluation asked while the Judge is off. Nothing
	// is sent.
	ReasonDisabled Reason = "disabled"
	// ReasonUnconfigured is an enabled Judge whose API key is not set or
	// whose settings are invalid.
	ReasonUnconfigured Reason = "unconfigured"
	// ReasonCoolingDown is an evaluation not sent while the provider recovers
	// from rate limits or an outage.
	ReasonCoolingDown Reason = "cooling_down"
	ReasonTimeout     Reason = "timeout"
	ReasonRateLimited Reason = "rate_limited"
	// ReasonUnavailable is a network failure or a server error.
	ReasonUnavailable Reason = "unavailable"
	// ReasonRejected is a request refused, before sending or by the
	// provider: bad credentials, an invalid or oversized request.
	ReasonRejected Reason = "rejected"
	// ReasonUnusable is a response that does not answer the questions in
	// the shape they require.
	ReasonUnusable Reason = "unusable"
	// ReasonCancelled is an evaluation whose context ended while it ran.
	ReasonCancelled Reason = "cancelled"
)

// transient reports whether another attempt may succeed shortly.
func (r Reason) transient() bool {
	return r == ReasonTimeout || r == ReasonUnavailable
}

// failure is a provider error in the Judge's terms.
type failure struct {
	reason     Reason
	retryAfter time.Duration
	detail     string
}

// failed translates a provider error. An error that is not a
// *systemone.Error is an unavailable provider.
func failed(err error) *failure {
	var e *systemone.Error
	if !errors.As(err, &e) {
		return &failure{reason: ReasonUnavailable, detail: err.Error()}
	}
	f := &failure{retryAfter: e.RetryAfter, detail: e.Message}
	switch e.Kind {
	case systemone.KindTimeout:
		f.reason = ReasonTimeout
	case systemone.KindRateLimited:
		f.reason = ReasonRateLimited
	case systemone.KindCancelled:
		f.reason = ReasonCancelled
	case systemone.KindMalformed:
		f.reason = ReasonUnusable
	case systemone.KindInvalidRequest, systemone.KindUnauthorized, systemone.KindRejected:
		f.reason = ReasonRejected
	default:
		f.reason = ReasonUnavailable
	}
	return f
}
