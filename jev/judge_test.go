package jev_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/jev"
	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
)

var at = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func enabled() jev.Settings {
	s := jev.DefaultSettings()
	s.Enabled, s.APIKeyEnv, s.Timeout = true, "TEST_JEV_KEY", time.Second
	return s
}

func judge(settings *jev.Settings, p *jevtest.Provider, key string) *jev.Judge {
	return &jev.Judge{
		Config:   func() jev.Settings { return *settings },
		Provider: p.Factory(),
		Getenv: func(name string) string {
			if name == "TEST_JEV_KEY" {
				return key
			}
			return ""
		},
		Now: func() time.Time { return at },
	}
}

func request(state string) systemone.Request {
	return systemone.Request{State: state, Questions: map[string]systemone.Question{
		"class": systemone.Choice("How did the agent end its turn?", systemone.Option{Name: "claims_done"}, systemone.Option{Name: "gave_up"}),
	}}
}

// awaitRequest blocks until p has received a request or a bounded deadline
// passes, so a provider that never receives one cannot hang the test.
func awaitRequest(p *jevtest.Provider) {
	deadline := time.Now().Add(5 * time.Second)
	for len(p.Requests()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}

func answer() jevtest.Result {
	return jevtest.Result{Response: systemone.Response{
		Model:   "jev-1.13.0",
		Answers: map[string]systemone.Answer{"class": jevtest.Choice("claims_done", 0.9, 0.85, "gave_up")},
		Usage:   systemone.Usage{InputTokens: 120, OutputTokens: 10, CostUSD: 0.000005, CostKnown: true},
	}}
}

func TestDisabledJudgeSendsNothing(t *testing.T) {
	settings := enabled()
	settings.Enabled = false
	p := &jevtest.Provider{Results: []jevtest.Result{answer()}}
	j := judge(&settings, p, "key")
	if d := j.Evaluate(context.Background(), request("Done.")); d.Outcome != jev.Fallback || d.Reason != jev.ReasonDisabled || len(p.Requests()) != 0 {
		t.Fatalf("decision = %+v, requests = %d", d, len(p.Requests()))
	}
	if s := j.Status(); s.Mode != jev.ModeDisabled {
		t.Fatalf("status = %+v", s)
	}
	var none *jev.Judge
	if d := none.Evaluate(context.Background(), request("Done.")); d.Reason != jev.ReasonDisabled {
		t.Fatalf("nil judge decision = %+v", d)
	}
	if s := none.Status(); s.Mode != jev.ModeDisabled {
		t.Fatalf("nil judge status = %+v", s)
	}
}

func TestAnsweredRequestIsAccepted(t *testing.T) {
	settings := enabled()
	p := &jevtest.Provider{Results: []jevtest.Result{answer()}}
	j := judge(&settings, p, "key")
	d := j.Evaluate(context.Background(), request("Done."))
	if d.Outcome != jev.Accepted || d.Reason != "" || d.Requests != 1 || d.Response == nil || d.Response.Model != "jev-1.13.0" || d.Response.Answers["class"].Choice != "claims_done" || !d.Response.Usage.CostKnown {
		t.Fatalf("decision = %+v", d)
	}
	if got := p.Requests(); len(got) != 1 || got[0].State != "Done." {
		t.Fatalf("requests = %+v", got)
	}
	if s := j.Status(); s.Mode != jev.ModeReady || s.Model != jev.DefaultModel {
		t.Fatalf("status = %+v", s)
	}
}

func TestFailuresFallBackWithBoundedRetries(t *testing.T) {
	for _, c := range []struct {
		name     string
		err      error
		want     jev.Reason
		requests int
	}{
		{"timeout retried once", &systemone.Error{Kind: systemone.KindTimeout}, jev.ReasonTimeout, 2},
		{"unavailable retried once", &systemone.Error{Kind: systemone.KindUnavailable}, jev.ReasonUnavailable, 2},
		{"other errors are unavailable", errors.New("connection reset"), jev.ReasonUnavailable, 2},
		{"unprocessable not retried", &systemone.Error{Kind: systemone.KindRejected, Message: "HTTP 422"}, jev.ReasonRejected, 1},
		{"unauthorized not retried", &systemone.Error{Kind: systemone.KindUnauthorized}, jev.ReasonRejected, 1},
		{"malformed not retried", &systemone.Error{Kind: systemone.KindMalformed}, jev.ReasonUnusable, 1},
		{"rate limit not retried", &systemone.Error{Kind: systemone.KindRateLimited}, jev.ReasonRateLimited, 1},
	} {
		settings := enabled()
		p := &jevtest.Provider{Results: []jevtest.Result{{Err: c.err}}}
		d := judge(&settings, p, "key").Evaluate(context.Background(), request("Done."))
		if d.Outcome != jev.Fallback || d.Reason != c.want || d.Requests != c.requests || len(p.Requests()) != c.requests || d.Response != nil {
			t.Errorf("%s: decision = %+v, requests = %d", c.name, d, len(p.Requests()))
		}
	}
}

func TestATransientFailureThenSuccessIsAccepted(t *testing.T) {
	settings := enabled()
	p := &jevtest.Provider{Results: []jevtest.Result{{Err: &systemone.Error{Kind: systemone.KindTimeout}}, answer()}}
	d := judge(&settings, p, "key").Evaluate(context.Background(), request("Done."))
	if d.Outcome != jev.Accepted || d.Requests != 2 || len(p.Requests()) != 2 {
		t.Fatalf("decision = %+v", d)
	}
}

func TestARateLimitCoolsEveryEvaluationDown(t *testing.T) {
	settings := enabled()
	p := &jevtest.Provider{Results: []jevtest.Result{{Err: &systemone.Error{Kind: systemone.KindRateLimited, RetryAfter: time.Minute}}}}
	j := judge(&settings, p, "key")
	j.Evaluate(context.Background(), request("first"))
	d := j.Evaluate(context.Background(), request("second"))
	if d.Reason != jev.ReasonCoolingDown || d.Requests != 0 || len(p.Requests()) != 1 {
		t.Fatalf("decision = %+v, requests = %d", d, len(p.Requests()))
	}
	s := j.Status()
	if s.Mode != jev.ModeDegraded || s.Reason != jev.ReasonRateLimited || s.CoolingUntil == nil || !s.CoolingUntil.Equal(at.Add(time.Minute)) {
		t.Fatalf("status = %+v", s)
	}
	settings.URL = "https://typesafe.example"
	if s := j.Status(); s.Mode != jev.ModeReady {
		t.Fatalf("changed settings kept the cool-down: %+v", s)
	}
}

func TestMissingKeyFallsBackVisibly(t *testing.T) {
	settings := enabled()
	p := &jevtest.Provider{Results: []jevtest.Result{answer()}}
	j := judge(&settings, p, "")
	d := j.Evaluate(context.Background(), request("Done."))
	if d.Reason != jev.ReasonUnconfigured || !strings.Contains(d.Detail, "TEST_JEV_KEY") || len(p.Requests()) != 0 {
		t.Fatalf("decision = %+v", d)
	}
	if s := j.Status(); s.Mode != jev.ModeUnconfigured {
		t.Fatalf("status = %+v", s)
	}
}

func TestInvalidSettingsFallBackWithoutARequest(t *testing.T) {
	for name, edit := range map[string]func(*jev.Settings){
		"no timeout":     func(s *jev.Settings) { s.Timeout = 0 },
		"long timeout":   func(s *jev.Settings) { s.Timeout = 2 * jev.MaxTimeout },
		"url with query": func(s *jev.Settings) { s.URL = "https://typesafe.example/?key=x" },
		"spaced model":   func(s *jev.Settings) { s.Model = "jev latest" },
		"bad key name":   func(s *jev.Settings) { s.APIKeyEnv = "1KEY" },
	} {
		settings := enabled()
		edit(&settings)
		p := &jevtest.Provider{Results: []jevtest.Result{answer()}}
		j := judge(&settings, p, "key")
		if d := j.Evaluate(context.Background(), request("Done.")); d.Reason != jev.ReasonUnconfigured || len(p.Requests()) != 0 {
			t.Errorf("%s: decision = %+v", name, d)
		}
		if s := j.Status(); s.Mode != jev.ModeUnconfigured {
			t.Errorf("%s: status = %+v", name, s)
		}
	}
	if err := jev.DefaultSettings().Validate(); err != nil {
		t.Fatalf("default settings: %v", err)
	}
}

func TestInvalidRequestsFallBackWithoutARequest(t *testing.T) {
	settings := enabled()
	p := &jevtest.Provider{Results: []jevtest.Result{answer()}}
	j := judge(&settings, p, "key")
	for name, r := range map[string]systemone.Request{
		"no state":    {Questions: request("x").Questions},
		"no question": {State: "Done."},
	} {
		if d := j.Evaluate(context.Background(), r); d.Reason != jev.ReasonRejected {
			t.Errorf("%s: decision = %+v", name, d)
		}
	}
	if len(p.Requests()) != 0 {
		t.Fatal("an invalid request was sent")
	}
}

func TestAnEndedContextCancelsWithoutCoolingDown(t *testing.T) {
	settings := enabled()
	blocking := &jevtest.Provider{Wait: jevtest.BlockUntilDone}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		awaitRequest(blocking)
		cancel()
	}()
	j := judge(&settings, blocking, "key")
	d := j.Evaluate(ctx, request("Done."))
	if d.Outcome != jev.Fallback || d.Reason != jev.ReasonCancelled || d.Requests != 1 {
		t.Fatalf("decision = %+v", d)
	}
	if s := j.Status(); s.Mode != jev.ModeReady {
		t.Fatalf("a cancelled request degraded the judge: %+v", s)
	}
}
