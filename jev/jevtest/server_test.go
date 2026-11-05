package jevtest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/jev"
	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
)

func serverRequest(state any) systemone.Request {
	return systemone.Request{State: state, Questions: map[string]systemone.Question{
		"choice": systemone.Choice("Choose a path", systemone.Option{Name: "accept"}, systemone.Option{Name: "review"}),
		"score":  systemone.Score("Rate the result", "low", "medium", "high"),
		"noul":   systemone.Noul("Is the task complete?"),
	}}
}

func serverResponse() systemone.Response {
	return systemone.Response{Model: "jev-scripted-version", Answers: map[string]systemone.Answer{
		"choice": jevtest.Choice("accept", 0.8, 0.7, "review"),
		"score":  jevtest.Score(1.2, 0.7, 0.1, 0.6, 0.3),
		"noul":   jevtest.Noul(0.95),
	}, Usage: systemone.Usage{InputTokens: 120, OutputTokens: 18, CostKnown: true, CostUSD: 0.002}}
}

func TestServerJudgeAnswersAndRecords(t *testing.T) {
	for _, usage := range []systemone.Usage{
		{InputTokens: 120, OutputTokens: 18, CostKnown: true, CostUSD: 0.002},
		{CostKnown: true},
		{},
	} {
		t.Run(fmt.Sprintf("usage-%+v", usage), func(t *testing.T) {
			response := serverResponse()
			response.Usage = usage
			// Zero score, probability and confidence must survive wire encoding too.
			response.Answers["score"] = jevtest.Score(0, 0, 1, 0, 0)
			response.Answers["noul"] = jevtest.Noul(0)
			server := jevtest.NewServer(t, jevtest.Result{Response: response})
			settings := server.Settings()
			settings.Model = "caller-model"
			if err := settings.Validate(); err != nil {
				t.Fatal(err)
			}
			endpoint, err := url.Parse(settings.URL)
			if err != nil {
				t.Fatal(err)
			}
			if !net.ParseIP(endpoint.Hostname()).IsLoopback() {
				t.Fatalf("endpoint = %s", settings.URL)
			}
			if settings.APIKeyEnv == jev.DefaultAPIKeyEnv || os.Getenv(settings.APIKeyEnv) != "jevtest-fake-key" {
				t.Fatalf("API key setting = %s", settings.APIKeyEnv)
			}
			judge := &jev.Judge{Config: func() jev.Settings { return settings }}
			request := serverRequest(map[string]any{"result": "done", "turns": float64(2)})
			decision := judge.Evaluate(context.Background(), request)
			if judge.Provider != nil || decision.Outcome != jev.Accepted || decision.Requests != 1 || !reflect.DeepEqual(decision.Response, &response) {
				t.Fatalf("decision = %+v, response = %+v", decision, decision.Response)
			}
			records := server.Requests()
			if len(records) != 1 {
				t.Fatalf("requests = %+v", records)
			}
			record := records[0]
			if record.Model != settings.Model || !record.Bearer || !reflect.DeepEqual(record.State, request.State) || !reflect.DeepEqual(record.QuestionIDs, []string{"choice", "noul", "score"}) {
				t.Fatalf("record = %+v", record)
			}
			body, err := request.Body(settings.Model)
			if err != nil {
				t.Fatal(err)
			}
			if string(record.Body) != string(body) {
				t.Fatalf("body = %s, want %s", record.Body, body)
			}
			// Callers may inspect and change snapshots without changing later reads.
			record.Body[0] = '!'
			record.QuestionIDs[0] = "changed"
			record.State.(map[string]any)["result"] = "changed"
			fresh := server.Requests()[0]
			if string(fresh.Body) != string(body) || fresh.QuestionIDs[0] != "choice" || !reflect.DeepEqual(fresh.State, request.State) {
				t.Fatalf("mutated record = %+v", fresh)
			}
		})
	}
}

func TestServerScriptOrderAndRetryRecovery(t *testing.T) {
	first, last := serverResponse(), serverResponse()
	last.Answers["noul"] = jevtest.Noul(0.25)
	server := jevtest.NewServer(t,
		jevtest.Result{Response: first},
		jevtest.Result{Err: &systemone.Error{Kind: systemone.KindUnavailable}},
		jevtest.Result{Response: last},
	)
	judge := &jev.Judge{Config: server.Settings}
	for i, step := range []struct {
		response systemone.Response
		requests int
	}{{first, 1}, {last, 2}, {last, 1}} {
		d := judge.Evaluate(context.Background(), serverRequest(fmt.Sprintf("turn %d", i)))
		if d.Outcome != jev.Accepted || d.Requests != step.requests || !reflect.DeepEqual(d.Response, &step.response) {
			t.Fatalf("evaluation %d = %+v, response = %+v", i, d, d.Response)
		}
	}
	records := server.Requests()
	want := []any{"turn 0", "turn 1", "turn 1", "turn 2"}
	if len(records) != len(want) {
		t.Fatalf("requests = %+v", records)
	}
	for i, r := range records {
		if r.State != want[i] || !r.Bearer {
			t.Fatalf("request %d = %+v", i, r)
		}
	}
	if status := judge.Status(); status.Mode != jev.ModeReady || status.Reason != "" || status.CoolingUntil != nil {
		t.Fatalf("status after recovery = %+v", status)
	}
}

func TestServerJudgeFailures(t *testing.T) {
	for _, tc := range []struct {
		kind     systemone.ErrorKind
		reason   jev.Reason
		requests []int
		cool     time.Duration
	}{
		{systemone.KindRateLimited, jev.ReasonRateLimited, []int{1, 0, 0}, 75 * time.Second},
		{systemone.KindUnauthorized, jev.ReasonRejected, []int{1, 1, 1}, 0},
		{systemone.KindUnavailable, jev.ReasonUnavailable, []int{2, 1, 0}, 30 * time.Second},
		{systemone.KindMalformed, jev.ReasonUnusable, []int{1, 1, 1}, 0},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			server := jevtest.NewServer(t, jevtest.Result{Err: &systemone.Error{Kind: tc.kind, RetryAfter: 75 * time.Second}})
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			judge := &jev.Judge{Config: server.Settings, Now: func() time.Time { return now }}
			total := 0
			for i, requests := range tc.requests {
				reason := tc.reason
				if requests == 0 {
					reason = jev.ReasonCoolingDown
				}
				d := judge.Evaluate(context.Background(), serverRequest("done"))
				total += requests
				if d.Outcome != jev.Fallback || d.Reason != reason || d.Requests != requests || d.Response != nil {
					t.Fatalf("evaluation %d = %+v", i, d)
				}
				if records := server.Requests(); len(records) != total {
					t.Fatalf("requests = %d, want %d", len(records), total)
				}
			}
			status := judge.Status()
			if status.Mode != jev.ModeDegraded || status.Reason != tc.reason {
				t.Fatalf("status = %+v", status)
			}
			if tc.cool == 0 {
				if status.CoolingUntil != nil {
					t.Fatalf("unexpected cool-down: %+v", status)
				}
			} else {
				until := now.Add(tc.cool)
				if status.CoolingUntil == nil || !status.CoolingUntil.Equal(until) {
					t.Fatalf("cooling until = %v, want %v", status.CoolingUntil, until)
				}
			}
		})
	}
}

func TestServerRateLimitExpires(t *testing.T) {
	response := serverResponse()
	server := jevtest.NewServer(t,
		jevtest.Result{Err: &systemone.Error{Kind: systemone.KindRateLimited, RetryAfter: 74*time.Second + time.Millisecond}},
		jevtest.Result{Response: response},
	)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	judge := &jev.Judge{Config: server.Settings, Now: func() time.Time { return now }}
	if d := judge.Evaluate(context.Background(), serverRequest("done")); d.Reason != jev.ReasonRateLimited || d.Requests != 1 {
		t.Fatalf("limited = %+v", d)
	}
	now = now.Add(74 * time.Second)
	if d := judge.Evaluate(context.Background(), serverRequest("done")); d.Reason != jev.ReasonCoolingDown || d.Requests != 0 {
		t.Fatalf("cooling = %+v", d)
	}
	now = now.Add(time.Second)
	if d := judge.Evaluate(context.Background(), serverRequest("done")); d.Outcome != jev.Accepted || d.Requests != 1 {
		t.Fatalf("recovered = %+v", d)
	}
	if len(server.Requests()) != 2 || judge.Status().Mode != jev.ModeReady {
		t.Fatalf("status = %+v, requests = %+v", judge.Status(), server.Requests())
	}
}

func TestServerEmptyScriptIsUnavailable(t *testing.T) {
	server := jevtest.NewServer(t)
	judge := &jev.Judge{Config: server.Settings}
	if d := judge.Evaluate(context.Background(), serverRequest("done")); d.Reason != jev.ReasonUnavailable || d.Requests != 2 {
		t.Fatalf("decision = %+v", d)
	}
	if len(server.Requests()) != 2 {
		t.Fatalf("requests = %+v", server.Requests())
	}
}

func TestServerRouteAndLifetime(t *testing.T) {
	var endpoint string
	t.Run("lifetime", func(t *testing.T) {
		response := serverResponse()
		server := jevtest.NewServer(t, jevtest.Result{Response: response}, jevtest.Result{Err: &systemone.Error{Kind: systemone.KindUnauthorized}})
		endpoint = server.Settings().URL
		for _, route := range []struct {
			method, path string
			status       int
		}{
			{http.MethodGet, "/v1/systemone", http.StatusMethodNotAllowed},
			{http.MethodPost, "/other", http.StatusNotFound},
		} {
			req, err := http.NewRequest(route.method, endpoint+route.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != route.status {
				t.Fatalf("%s %s: %d", route.method, route.path, resp.StatusCode)
			}
		}
		if len(server.Requests()) != 0 {
			t.Fatalf("requests = %+v", server.Requests())
		}
		// Direct HTTP callers can also use the endpoint; absent credentials are recorded.
		body, err := serverRequest("direct").Body("direct-model")
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.Post(endpoint+"/v1/systemone", "application/json", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK || !json.Valid(data) {
			t.Fatalf("response = %d %s", resp.StatusCode, data)
		}
		records := server.Requests()
		if len(records) != 1 || records[0].Bearer || records[0].State != "direct" {
			t.Fatalf("requests = %+v", records)
		}
	})
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(endpoint + "/v1/systemone")
	if err == nil {
		resp.Body.Close()
		t.Fatal("server remains reachable after cleanup")
	}
}
