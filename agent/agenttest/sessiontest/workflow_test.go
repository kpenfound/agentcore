package sessiontest_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest"
	"github.com/kpenfound/agentcore/jev"
	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
	"github.com/kpenfound/agentcore/vcs"
)

func TestCallerWorkflow(t *testing.T) {
	claude := agenttest.Session(t, agent.AgentClaude,
		agenttest.Turn{ResultText: "claude draft", SessionID: "claude-1"},
		agenttest.Turn{ResultText: "cannot draft", End: agenttest.TurnError, ErrorSubtype: "error_max_turns", ExitCode: 7},
	)
	codex := agenttest.Session(t, agent.AgentCodex,
		agenttest.Turn{ResultText: "codex draft", SessionID: "codex-1"},
		agenttest.Turn{ResultText: "draft needing manual review", SessionID: "codex-2"},
	)
	accept := jevtest.Result{Response: systemone.Response{Model: "scripted-review", Answers: map[string]systemone.Answer{
		"route": jevtest.Choice("publish", 0.9, 0.9, "review"),
	}}}
	endpoint := jevtest.NewServer(t, accept, accept,
		jevtest.Result{Err: &systemone.Error{Kind: systemone.KindUnavailable}},
	)
	judge := &jev.Judge{Config: endpoint.Settings} // Provider stays nil: use the HTTP client.
	runner := &agent.Runner{ClaudeBin: claude.Bin, CodexBin: codex.Bin, SessionsDir: t.TempDir()}
	workspace := t.TempDir()
	var published, manualReview []string
	var failed []*agent.Result

	// This routing is caller policy. Only the external executables and endpoint
	// are faked; Runner, Judge and the caller's actions run here.
	run := func(backend, prompt string) (*agent.Result, jev.Decision) {
		t.Helper()
		result, err := runner.Run(context.Background(), agent.Request{
			Name: "draft", Prompt: prompt, Workspace: vcs.Directory(workspace),
			Profile: agent.Profile{Agent: backend, Timeout: 5 * time.Second, VCSAccess: true},
			Grants: &agent.Grants{VCS: true, Tools: []string{agent.ToolsAll}, Mounts: []agent.Mount{
				{Path: "/", Access: agent.ReadWrite},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			failed = append(failed, result)
			return result, jev.Decision{}
		}
		decision := judge.Evaluate(context.Background(), systemone.Request{
			State: result.ResultText,
			Questions: map[string]systemone.Question{"route": systemone.Choice(
				"Publish this draft or send it for review?",
				systemone.Option{Name: "publish"}, systemone.Option{Name: "review"},
			)},
		})
		if decision.Outcome == jev.Accepted && decision.Response.Answers["route"].Choice == "publish" {
			published = append(published, result.ResultText)
		} else {
			manualReview = append(manualReview, result.ResultText)
		}
		return result, decision
	}

	for _, step := range []struct {
		backend, prompt, text string
		outcome               jev.Outcome
		reason                jev.Reason
		requests              int
		isError               bool
	}{
		{backend: agent.AgentClaude, prompt: "Draft with claude", text: "claude draft", outcome: jev.Accepted, requests: 1},
		{backend: agent.AgentCodex, prompt: "Draft with codex", text: "codex draft", outcome: jev.Accepted, requests: 1},
		{backend: agent.AgentCodex, prompt: "Draft during outage", text: "draft needing manual review", outcome: jev.Fallback, reason: jev.ReasonUnavailable, requests: 2},
		{backend: agent.AgentClaude, prompt: "Draft that fails", text: "cannot draft", isError: true},
	} {
		result, decision := run(step.backend, step.prompt)
		if result.ResultText != step.text || result.IsError != step.isError {
			t.Fatalf("%s: result = %+v", step.prompt, result)
		}
		if decision.Outcome != step.outcome || decision.Reason != step.reason || decision.Requests != step.requests {
			t.Fatalf("%s: decision = %+v", step.prompt, decision)
		}
	}
	if !slices.Equal(published, []string{"claude draft", "codex draft"}) || !slices.Equal(manualReview, []string{"draft needing manual review"}) {
		t.Fatalf("published = %q, manual review = %q", published, manualReview)
	}
	if len(failed) != 1 || !failed[0].IsError || failed[0].ErrorSubtype != "error_max_turns" || failed[0].ExitCode != 7 {
		t.Fatalf("agent failures = %+v", failed)
	}
	for _, fixture := range []struct {
		session *agenttest.SessionFixture
		prompts []string
	}{
		{claude, []string{"Draft with claude", "Draft that fails"}},
		{codex, []string{"Draft with codex", "Draft during outage"}},
	} {
		record := fixture.session.Record(t)
		if record.Starts != 2 || len(record.Sessions) != 2 {
			t.Fatalf("agent record = %+v", record)
		}
		for i, invocation := range record.Sessions {
			if strings.TrimSuffix(invocation.Prompt, "\n") != fixture.prompts[i] || invocation.WorkingDirectory != workspace {
				t.Fatalf("session %d = %+v", i, invocation)
			}
		}
	}
	requests := endpoint.Requests()
	wantStates := []string{"claude draft", "codex draft", "draft needing manual review", "draft needing manual review"}
	if len(requests) != len(wantStates) {
		t.Fatalf("Jev requests = %+v", requests)
	}
	for i, request := range requests {
		if request.State != wantStates[i] || request.Model != endpoint.Settings().Model || !request.Bearer || !slices.Equal(request.QuestionIDs, []string{"route"}) {
			t.Fatalf("Jev request %d = %+v", i, request)
		}
	}
}
