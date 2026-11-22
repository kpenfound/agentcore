package agent_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest"
	"github.com/kpenfound/agentcore/skills"
	"github.com/kpenfound/agentcore/vcs"
)

// Skill repositories the tests "clone": each URL is answered with a copy of
// a local fixture directory, so nothing reaches the network.
const (
	singleSkillURL    = "https://example.test/acme/single"
	collectionURL     = "https://example.test/acme/collection"
	nativePluginURL   = "https://example.test/acme/native"
	bareNativePlugURL = "https://example.test/acme/bare-native"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// skillRepositories builds the four fixture layouts and a skills.Manager
// whose git clones copy them.
func skillRepositories(t *testing.T) *skills.Manager {
	t.Helper()
	fixtures := map[string]string{}
	repo := func(url string, files map[string]string) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, ".git", "HEAD"), "")
		for name, content := range files {
			writeFile(t, filepath.Join(dir, name), content)
		}
		fixtures[url] = dir
	}
	repo(singleSkillURL, map[string]string{"SKILL.md": "---\nname: single\n---\nsingle body\n"})
	repo(collectionURL, map[string]string{
		"skills/alpha/SKILL.md": "---\nname: alpha\n---\nalpha body\n",
		"skills/beta/SKILL.md":  "---\nname: beta\n---\nbeta body\n",
	})
	repo(nativePluginURL, map[string]string{
		".claude-plugin/plugin.json": `{"name":"native"}`,
		"skills/tdd/SKILL.md":        "---\nname: tdd\n---\ntdd body\n",
	})
	repo(bareNativePlugURL, map[string]string{
		".claude-plugin/plugin.json": `{"name":"bare"}`,
		"hooks/hooks.json":           `{}`,
	})
	m := skills.NewManager(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.Git = func(_ context.Context, _ string, args ...string) (string, error) {
		switch args[0] {
		case "clone":
			return "", os.CopyFS(args[len(args)-1], os.DirFS(fixtures[args[len(args)-2]]))
		case "rev-parse":
			return "abc1234\n", nil
		}
		return "", nil
	}
	return m
}

// codexSkillsTurn is a host-placed codex turn for the fake app-server naming
// refs as its skills.
func codexSkillsTurn(t *testing.T, m *skills.Manager, bin string, refs ...string) (*agent.Runner, agent.Request) {
	t.Helper()
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &agent.Runner{CodexBin: bin, SessionsDir: t.TempDir(), Skills: m}
	req := agent.Request{
		Name:      "n",
		Profile:   agent.Profile{Name: "builder", Agent: agent.AgentCodex, Skills: refs, VCSAccess: true},
		Workspace: vcs.Directory(work),
		Prompt:    "TASK",
		Grants: &agent.Grants{
			Env:    []string{"PATH", "HOME", "TMPDIR", "FAKE_CODEX_APP_SERVER*"},
			Tools:  []string{agent.ToolsAll},
			VCS:    true,
			Mounts: []agent.Mount{{Path: "/", Access: agent.ReadWrite}},
		},
	}
	return r, req
}

func completedTurn() []agenttest.CodexAppServerAction {
	return []agenttest.CodexAppServerAction{
		{Notify: &agenttest.CodexAppServerMessage{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"status": "completed"}}}},
	}
}

// recordedConversation is the methods the fake received and the roots of
// its skills/extraRoots/set requests.
func recordedConversation(t *testing.T, record string) (methods []string, roots [][]string) {
	t.Helper()
	rec := agenttest.ReadCodexAppServerRecord(t, record)
	for _, line := range rec.Lines {
		var l struct {
			Method string `json:"method"`
			Params struct {
				ExtraRoots []string `json:"extraRoots"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatal(err)
		}
		if l.Method == "" {
			continue
		}
		methods = append(methods, l.Method)
		if l.Method == "skills/extraRoots/set" {
			roots = append(roots, l.Params.ExtraRoots)
		}
	}
	return methods, roots
}

// skillBodies reads, from a registered root, the SKILL.md of every skill
// directly under it, by skill directory name.
func skillBodies(t *testing.T, root string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("registered root %s: %v", root, err)
	}
	bodies := map[string]string{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(root, e.Name(), "SKILL.md"))
		if err != nil {
			t.Fatalf("skill %s under %s: %v", e.Name(), root, err)
		}
		bodies[e.Name()] = string(data)
	}
	return bodies
}

// TestCodexRegistersTheRootsOfEverySkillLayout: skills prepared by a real
// skills.Manager, run through Runner, give registered roots that lead on
// disk to the configured skills' SKILL.md files.
func TestCodexRegistersTheRootsOfEverySkillLayout(t *testing.T) {
	for _, tc := range []struct {
		name string
		refs []string
		want []map[string]string // per registered root, skill name to SKILL.md body
	}{
		{"single-skill wrapper", []string{singleSkillURL}, []map[string]string{{"single": "---\nname: single\n---\nsingle body\n"}}},
		{"collection wrapper", []string{collectionURL}, []map[string]string{{"alpha": "---\nname: alpha\n---\nalpha body\n", "beta": "---\nname: beta\n---\nbeta body\n"}}},
		{"native plugin with skills", []string{nativePluginURL}, []map[string]string{{"tdd": "---\nname: tdd\n---\ntdd body\n"}}},
		{"all three together", []string{singleSkillURL, collectionURL, nativePluginURL}, []map[string]string{
			{"single": "---\nname: single\n---\nsingle body\n"},
			{"alpha": "---\nname: alpha\n---\nalpha body\n", "beta": "---\nname: beta\n---\nbeta body\n"},
			{"tdd": "---\nname: tdd\n---\ntdd body\n"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, record := agenttest.CodexAppServer(t, agenttest.CodexAppServerScript{ThreadID: "thread-m", Actions: completedTurn()})
			r, req := codexSkillsTurn(t, skillRepositories(t), bin, tc.refs...)
			res, err := r.Run(context.Background(), req)
			if err != nil || res.IsError {
				t.Fatalf("run: %+v, %v", res, err)
			}
			methods, roots := recordedConversation(t, record)
			if want := []string{"initialize", "initialized", "skills/extraRoots/set", "thread/start", "turn/start"}; !slices.Equal(methods, want) {
				t.Fatalf("conversation = %v, want %v", methods, want)
			}
			if len(roots) != 1 || len(roots[0]) != len(tc.want) {
				t.Fatalf("extraRoots = %v, want %d roots", roots, len(tc.want))
			}
			for i, root := range roots[0] {
				if !filepath.IsAbs(root) || filepath.Base(root) != "skills" {
					t.Errorf("root %q is not an absolute skills directory", root)
				}
				got := skillBodies(t, root)
				if len(got) != len(tc.want[i]) {
					t.Errorf("root %s holds %v, want %v", root, got, tc.want[i])
				}
				for name, body := range tc.want[i] {
					if got[name] != body {
						t.Errorf("root %s: skill %s = %q, want %q", root, name, got[name], body)
					}
				}
			}
		})
	}
}

// TestCodexNativePluginWithoutSkillsFailsTheTurn: a native plugin with no
// skills directory has nothing to register, so the turn fails naming
// skills, before the app-server starts and so before any thread request.
func TestCodexNativePluginWithoutSkillsFailsTheTurn(t *testing.T) {
	bin, record := agenttest.CodexAppServer(t, agenttest.CodexAppServerScript{Actions: completedTurn()})
	r, req := codexSkillsTurn(t, skillRepositories(t), bin, singleSkillURL, bareNativePlugURL)
	_, err := r.Run(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "skills") {
		t.Fatalf("error = %v, want one naming skills", err)
	}
	if _, statErr := os.Stat(record); statErr == nil {
		methods, _ := recordedConversation(t, record)
		t.Fatalf("the app-server ran: %v", methods)
	}
}

// TestRunRestrictedRegistersManagerSkills: the same layouts through
// Runner.RunRestricted.
func TestRunRestrictedRegistersManagerSkills(t *testing.T) {
	bin, record := agenttest.CodexAppServer(t, agenttest.CodexAppServerScript{
		ThreadID: "thread-m",
		Actions: []agenttest.CodexAppServerAction{
			{Notify: &agenttest.CodexAppServerMessage{Method: "item/completed", Params: map[string]any{"item": map[string]any{"type": "agentMessage", "text": "done"}}}},
			{Notify: &agenttest.CodexAppServerMessage{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"status": "completed"}}}},
		},
	})
	m := skillRepositories(t)
	r := &agent.Runner{CodexBin: bin, SessionsDir: t.TempDir(), Skills: m}
	res, err := r.RunRestricted(context.Background(), agent.Request{
		Name:      "reviewer",
		Profile:   agent.Profile{Name: "reviewer", Agent: agent.AgentCodex, Skills: []string{collectionURL}},
		Workspace: vcs.Directory(t.TempDir()),
		Prompt:    "review",
	})
	if err != nil || res.ResultText != "done" {
		t.Fatalf("run: %+v, %v", res, err)
	}
	methods, roots := recordedConversation(t, record)
	if want := []string{"initialize", "initialized", "skills/extraRoots/set", "thread/start", "turn/start"}; !slices.Equal(methods, want) {
		t.Fatalf("conversation = %v, want %v", methods, want)
	}
	if len(roots) != 1 || len(roots[0]) != 1 {
		t.Fatalf("extraRoots = %v", roots)
	}
	if got := skillBodies(t, roots[0][0]); len(got) != 2 || !strings.Contains(got["alpha"], "alpha body") {
		t.Errorf("registered root holds %v", got)
	}
}
