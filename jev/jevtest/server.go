package jevtest

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/jev"
	"github.com/kpenfound/agentcore/jev/systemone"
)

// Request records what reached a Server. State is decoded JSON (objects are
// map[string]any and numbers are float64). QuestionIDs are sorted. Body holds
// the full JSON request, including question instructions and criteria. Bearer
// reports whether a nonempty bearer credential was presented, never its value.
type Request struct {
	Model       string
	State       any
	QuestionIDs []string
	Bearer      bool
	Body        []byte
}

type serverResult struct {
	status     int
	retryAfter string
	body       []byte
}

// Server serves scripted System One results on 127.0.0.1. Successive POSTs to
// /v1/systemone take results in order and repeat the last, including retries.
// An empty script returns unavailable. Use Settings with a real jev.Judge and
// leave its Provider nil to exercise the HTTP client.
type Server struct {
	settings jev.Settings
	results  []serverResult
	mu       sync.Mutex
	requests []Request
}

var serverSequence atomic.Uint64

// NewServer starts a loopback endpoint and closes it at test cleanup. It sets
// a unique JEVTEST_API_KEY_* environment variable to an obviously fake key;
// Settings names that variable without touching a production key. Like
// testing.T.Setenv, it cannot be used in a parallel test or under one.
//
// Results are captured at construction. Rate-limited, unauthorized, unavailable
// and malformed systemone.Errors become HTTP 429, 401, 503 and invalid JSON
// with HTTP 200 respectively. Invalid-request and rejected errors become 400;
// other errors become 503. Timeout and cancellation must be driven by the
// caller's context, and are refused here. RetryAfter is rounded up to whole
// seconds for the HTTP header. Error messages and StatusCode are not served.
func NewServer(t testing.TB, results ...Result) *Server {
	t.Helper()
	s := &Server{}
	if len(results) == 0 {
		results = []Result{{Err: &systemone.Error{Kind: systemone.KindUnavailable}}}
	}
	for _, result := range results {
		prepared, err := prepareResult(result)
		if err != nil {
			t.Fatalf("jevtest server: %v", err)
		}
		s.results = append(s.results, prepared)
	}
	endpoint := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(endpoint.Close)
	s.settings = jev.DefaultSettings()
	s.settings.Enabled = true
	s.settings.URL = endpoint.URL
	s.settings.APIKeyEnv = "JEVTEST_API_KEY_" + strconv.FormatUint(serverSequence.Add(1), 10)
	t.Setenv(s.settings.APIKeyEnv, "jevtest-fake-key")
	return s
}

// Settings returns enabled settings pointing only at the loopback endpoint.
// The caller can change the returned model or timeout for its Judge.
func (s *Server) Settings() jev.Settings { return s.settings }

// Requests returns an independent snapshot of every POST to /v1/systemone,
// including retries. Other paths and methods do not consume scripted results.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	requests := make([]Request, len(s.requests))
	for i, r := range s.requests {
		requests[i], _ = decodeRequest(slices.Clone(r.Body), r.Bearer)
	}
	return requests
}

func decodeRequest(body []byte, bearer bool) (Request, error) {
	var wire struct {
		Model     string                     `json:"model"`
		State     any                        `json:"state"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	err := json.Unmarshal(body, &wire)
	r := Request{Model: wire.Model, State: wire.State, Bearer: bearer, Body: body}
	for id := range wire.Questions {
		r.QuestionIDs = append(r.QuestionIDs, id)
	}
	slices.Sort(r.QuestionIDs)
	return r, err
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/systemone" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "cannot read request", http.StatusBadRequest)
		return
	}
	auth := strings.Fields(r.Header.Get("Authorization"))
	bearer := len(auth) == 2 && strings.EqualFold(auth[0], "Bearer")
	record, err := decodeRequest(body, bearer)
	s.mu.Lock()
	s.requests = append(s.requests, record)
	result := s.results[min(len(s.requests), len(s.results))-1]
	s.mu.Unlock()
	if err != nil {
		http.Error(w, "request is not JSON", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if result.retryAfter != "" {
		w.Header().Set("Retry-After", result.retryAfter)
	}
	w.WriteHeader(result.status)
	_, _ = w.Write(result.body)
}

func prepareResult(result Result) (serverResult, error) {
	out := serverResult{status: http.StatusOK}
	if result.Err != nil {
		out.status = http.StatusServiceUnavailable
		var e *systemone.Error
		if errors.As(result.Err, &e) {
			switch e.Kind {
			case systemone.KindRateLimited:
				out.status = http.StatusTooManyRequests
				if e.RetryAfter > 0 {
					seconds := e.RetryAfter / time.Second
					if e.RetryAfter%time.Second != 0 {
						seconds++
					}
					out.retryAfter = strconv.FormatInt(int64(seconds), 10)
				}
			case systemone.KindUnauthorized:
				out.status = http.StatusUnauthorized
			case systemone.KindInvalidRequest, systemone.KindRejected:
				out.status = http.StatusBadRequest
			case systemone.KindMalformed:
				out.status, out.body = http.StatusOK, []byte("{malformed")
			case systemone.KindTimeout, systemone.KindCancelled:
				return out, errors.New("timeouts and cancellation are not scripted HTTP responses")
			}
		}
		return out, nil
	}
	// Answer.MarshalJSON already writes the wire fields for its kind. Usage
	// needs the API's optional cost field, rather than Usage's cost_usd field.
	var cost *float64
	if result.Response.Usage.CostKnown {
		cost = &result.Response.Usage.CostUSD
	}
	type wireUsage struct {
		InputTokens  int      `json:"input_tokens"`
		OutputTokens int      `json:"output_tokens"`
		Cost         *float64 `json:"cost,omitempty"`
	}
	body, err := json.Marshal(struct {
		Model   string                      `json:"model"`
		Answers map[string]systemone.Answer `json:"answers"`
		Usage   wireUsage                   `json:"usage"`
	}{
		Model: result.Response.Model, Answers: result.Response.Answers,
		Usage: wireUsage{result.Response.Usage.InputTokens, result.Response.Usage.OutputTokens, cost},
	})
	out.body = body
	return out, err
}
