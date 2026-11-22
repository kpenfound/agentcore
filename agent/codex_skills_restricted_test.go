package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/agent/agenttest"
)

// TestRunRestrictedRegistersCodexSkills: a restricted codex turn whose
// profile names skills registers them before thread/start, under the same
// read-only floor, and completes.
func TestRunRestrictedRegistersCodexSkills(t *testing.T) {
	bin, record := agenttest.CodexAppServer(t, restrictedCodexScript(agenttest.CodexAppServerScript{}))
	plugin := skillPlugin(t, "pr-review")
	prep := &recordingSkills{dirs: []string{plugin}}
	r := restrictedRunner(t, "", bin)
	r.Skills = prep
	r.SkillMountDirs = []string{plugin}
	req := restrictedRequestFor(AgentCodex, t.TempDir())
	req.Profile.Skills = []string{"ref"}
	res, err := r.RunRestricted(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.ResultText != "the brief" || res.Agent != AgentCodex {
		t.Fatalf("result = %+v", res)
	}
	if prep.callCount() != 1 {
		t.Errorf("Prepare called %d times", prep.callCount())
	}
	rec := agenttest.ReadCodexAppServerRecord(t, record)
	want := []string{"initialize", "initialized", "skills/extraRoots/set", "thread/start", "turn/start"}
	if got := codexRecordedMethods(t, rec); !slices.Equal(got, want) {
		t.Fatalf("conversation = %v, want %v", got, want)
	}
	if roots := codexRecordedSkillRoots(t, rec); len(roots) != 1 || !slices.Equal(roots[0], []string{filepath.Join(plugin, "skills")}) {
		t.Fatalf("extraRoots = %v", roots)
	}
	args, _ := restrictedCodexStart(t, rec, "app-server")
	for _, want := range []string{`approval_policy="never"`, "agents.enabled=false", "features.shell_tool=false"} {
		if !slices.Contains(args, want) {
			t.Errorf("args lost %q: %v", want, args)
		}
	}
	if start := codexRecordedThreadStart(t, rec); start.Sandbox != "read-only" {
		t.Errorf("sandbox = %q, want read-only", start.Sandbox)
	}
}

// TestRunRestrictedRegistersSkillsOfACodexFallback: skills on the codex
// fallback profile are registered when the turn falls back to it.
func TestRunRestrictedRegistersSkillsOfACodexFallback(t *testing.T) {
	limited, _ := restrictedFake(t, "claude", `echo '{"type":"result","subtype":"error","is_error":true,"result":"rate limit reached","session_id":"s0"}'`)
	answering, record := agenttest.CodexAppServer(t, restrictedCodexScript(agenttest.CodexAppServerScript{}))
	plugin := skillPlugin(t, "pr-review")
	prep := &recordingSkills{dirs: []string{plugin}}
	r := restrictedRunner(t, limited, answering)
	r.Skills = prep
	req := restrictedRequestFor(AgentClaude, t.TempDir())
	req.Profile.Fallback = &Profile{Name: "reviewer", Agent: AgentCodex, Model: "gpt-last", Skills: []string{"ref"}}
	res, err := r.RunRestricted(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Agent != AgentCodex || res.Model != "gpt-last" {
		t.Fatalf("result = %+v", res)
	}
	rec := agenttest.ReadCodexAppServerRecord(t, record)
	want := []string{"initialize", "initialized", "skills/extraRoots/set", "thread/start", "turn/start"}
	if got := codexRecordedMethods(t, rec); !slices.Equal(got, want) {
		t.Fatalf("conversation = %v, want %v", got, want)
	}
	if roots := codexRecordedSkillRoots(t, rec); len(roots) != 1 || !slices.Equal(roots[0], []string{filepath.Join(plugin, "skills")}) {
		t.Fatalf("extraRoots = %v", roots)
	}
}

// TestRunRestrictedFailsClosedOnCodexSkills: a restricted codex turn with
// skills never starts a thread when the registration is rejected or the
// runner cannot prepare them.
func TestRunRestrictedFailsClosedOnCodexSkills(t *testing.T) {
	t.Run("rejected", func(t *testing.T) {
		bin, record := agenttest.CodexAppServer(t, restrictedCodexScript(agenttest.CodexAppServerScript{
			SkillsError: &agenttest.CodexRPCError{Code: -32601, Message: "method not found"},
		}))
		r := restrictedRunner(t, "", bin)
		r.Skills = &recordingSkills{dirs: []string{skillPlugin(t, "a")}}
		req := restrictedRequestFor(AgentCodex, t.TempDir())
		req.Profile.Skills = []string{"ref"}
		_, err := r.RunRestricted(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), "skills/extraRoots/set") || !strings.Contains(err.Error(), "method not found") {
			t.Fatalf("error = %v", err)
		}
		assertNoThreadRequest(t, agenttest.ReadCodexAppServerRecord(t, record))
	})
	t.Run("no preparer", func(t *testing.T) {
		bin, record := agenttest.CodexAppServer(t, restrictedCodexScript(agenttest.CodexAppServerScript{}))
		r := restrictedRunner(t, "", bin)
		req := restrictedRequestFor(AgentCodex, t.TempDir())
		req.Profile.Skills = []string{"ref"}
		_, err := r.RunRestricted(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), "skills") {
			t.Fatalf("error = %v", err)
		}
		if codexRecordedAppServerStarts(t, record) != 0 {
			t.Error("the app-server started without skills")
		}
	})
}

// codexRecordedAppServerStarts counts the app-server starts in a record,
// not the `mcp list` inventory a restricted turn runs apart from it.
func codexRecordedAppServerStarts(t *testing.T, record string) int {
	t.Helper()
	if _, err := os.Stat(record); errors.Is(err, os.ErrNotExist) {
		return 0
	}
	n := 0
	for _, argv := range agenttest.ReadCodexAppServerRecord(t, record).Argv {
		if len(argv) > 1 && argv[1] == "app-server" {
			n++
		}
	}
	return n
}

// TestRunRestrictedRejectsSkillsOnOtherBackends: skills on a claude,
// opencode or pi profile, primary or fallback, are refused before anything
// launches; a codex primary or fallback in the same chain does not change
// that.
func TestRunRestrictedRejectsSkillsOnOtherBackends(t *testing.T) {
	bin, record := restrictedFake(t, "claude", restrictedClaudeAnswer)
	r := restrictedRunner(t, bin, "")
	r.Skills = &recordingSkills{dirs: []string{skillPlugin(t, "a")}}
	for _, agentName := range []string{AgentClaude, AgentOpenCode, AgentPi} {
		t.Run(agentName+" primary", func(t *testing.T) {
			req := restrictedRequestFor(agentName, t.TempDir())
			req.Profile.Skills = []string{"ref"}
			assertSkillsRejected(t, r, req)
		})
		t.Run(agentName+" fallback of codex", func(t *testing.T) {
			req := restrictedRequestFor(AgentCodex, t.TempDir())
			req.Profile.Skills = []string{"ref"}
			req.Profile.Fallback = &Profile{Name: "reviewer", Agent: agentName, Skills: []string{"ref"}}
			assertSkillsRejected(t, r, req)
		})
		t.Run(agentName+" fallback of claude", func(t *testing.T) {
			req := restrictedRequestFor(AgentClaude, t.TempDir())
			req.Profile.Fallback = &Profile{Name: "reviewer", Agent: agentName, Skills: []string{"ref"}}
			assertSkillsRejected(t, r, req)
		})
	}
	if _, err := os.Stat(record + ".args"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("agent launched for a refused request: %v", err)
	}
}

func assertSkillsRejected(t *testing.T, r *Runner, req Request) {
	t.Helper()
	_, err := r.RunRestricted(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "skill") {
		t.Fatalf("error = %v, want a refusal naming skills", err)
	}
}

// TestRunRestrictedKeepsItsOtherRestrictionsForCodexSkills: a codex profile
// with skills is still refused for tools, MCP, a shell, VCS, packages and a
// caller-selected sandbox, on the primary and on a fallback, before the
// app-server starts.
func TestRunRestrictedKeepsItsOtherRestrictionsForCodexSkills(t *testing.T) {
	bin, record := agenttest.CodexAppServer(t, restrictedCodexScript(agenttest.CodexAppServerScript{}))
	r := restrictedRunner(t, "", bin)
	prep := &recordingSkills{dirs: []string{skillPlugin(t, "a")}}
	r.Skills = prep
	for _, tc := range []struct {
		name   string
		change func(*Profile)
	}{
		{"MCP", func(p *Profile) { p.MCP = map[string]MCPEntry{"tools": {Command: "server"}} }},
		{"allowed tools", func(p *Profile) { p.AllowedTools = []string{"Bash"} }},
		{"disallowed tools", func(p *Profile) { p.DisallowedTools = []string{"Bash"} }},
		{"shell", func(p *Profile) { p.Shell = "/bin/sh" }},
		{"VCS", func(p *Profile) { p.VCSAccess = true }},
		{"packages", func(p *Profile) { p.PiPackages = []string{"npm:x"} }},
		{"container sandbox", func(p *Profile) { p.Sandbox, p.SandboxImage = SandboxContainer, "image" }},
		{"confine", func(p *Profile) { p.Confine = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			primary := restrictedRequestFor(AgentCodex, t.TempDir())
			primary.Profile.Skills = []string{"ref"}
			tc.change(&primary.Profile)
			if _, err := r.RunRestricted(context.Background(), primary); err == nil {
				t.Error("primary: unsafe request was accepted")
			}
			fallback := restrictedRequestFor(AgentCodex, t.TempDir())
			fallback.Profile.Fallback = &Profile{Name: "reviewer", Agent: AgentCodex, Skills: []string{"ref"}}
			tc.change(fallback.Profile.Fallback)
			if _, err := r.RunRestricted(context.Background(), fallback); err == nil {
				t.Error("fallback: unsafe request was accepted")
			}
		})
	}
	req := restrictedRequestFor(AgentCodex, t.TempDir())
	req.Profile.Skills = []string{"ref"}
	req.Grants = &Grants{}
	if _, err := r.RunRestricted(context.Background(), req); err == nil {
		t.Error("caller grants were accepted")
	}
	if prep.callCount() != 0 || codexRecordedAppServerStarts(t, record) != 0 {
		t.Errorf("a refused request prepared skills (%d) or started the app-server", prep.callCount())
	}
}

// TestRunRestrictedPreparesNothingWithoutSkills: a runner with a preparer
// but a profile chain naming no skills never calls it.
func TestRunRestrictedPreparesNothingWithoutSkills(t *testing.T) {
	bin, record := agenttest.CodexAppServer(t, restrictedCodexScript(agenttest.CodexAppServerScript{}))
	prep := &recordingSkills{dirs: []string{skillPlugin(t, "a")}}
	r := restrictedRunner(t, "", bin)
	r.Skills = prep
	if _, err := r.RunRestricted(context.Background(), restrictedRequestFor(AgentCodex, t.TempDir())); err != nil {
		t.Fatal(err)
	}
	if prep.callCount() != 0 {
		t.Errorf("Prepare called %d times", prep.callCount())
	}
	rec := agenttest.ReadCodexAppServerRecord(t, record)
	if n := codexSkillRequestCount(t, rec); n != 0 {
		t.Errorf("%d skills/extraRoots/set requests", n)
	}
}
