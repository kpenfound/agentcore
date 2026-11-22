package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/agentcore/agent/agenttest"
)

// recordingSkills is a SkillPreparer that returns fixed plugin directories
// and records every call.
type recordingSkills struct {
	dirs []string
	err  error

	mu    sync.Mutex
	calls [][]string
}

func (p *recordingSkills) Prepare(_ context.Context, refs []string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, slices.Clone(refs))
	return p.dirs, p.err
}

func (p *recordingSkills) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// skillPlugin makes a plugin directory holding skills/<name>/SKILL.md for
// each name and returns it.
func skillPlugin(t *testing.T, names ...string) string {
	t.Helper()
	dir := realTempDir(t)
	for _, name := range names {
		skill := filepath.Join(dir, "skills", name)
		if err := os.MkdirAll(skill, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: "+name+"\n---\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// codexRecordedMethods is the method of every line the fake received that
// has one, in order: requests and notifications, not the driver's answers.
func codexRecordedMethods(t *testing.T, rec agenttest.CodexAppServerRecord) []string {
	t.Helper()
	var methods []string
	for _, line := range rec.Lines {
		var l struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("record line %q: %v", line, err)
		}
		if l.Method != "" {
			methods = append(methods, l.Method)
		}
	}
	return methods
}

// codexRecordedSkillRoots is the extraRoots of every skills/extraRoots/set
// request the fake received.
func codexRecordedSkillRoots(t *testing.T, rec agenttest.CodexAppServerRecord) [][]string {
	t.Helper()
	var all [][]string
	for _, line := range rec.Lines {
		var l struct {
			Method string `json:"method"`
			Params struct {
				ExtraRoots []string `json:"extraRoots"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("record line %q: %v", line, err)
		}
		if l.Method == "skills/extraRoots/set" {
			all = append(all, l.Params.ExtraRoots)
		}
	}
	return all
}

func codexSkillRequestCount(t *testing.T, rec agenttest.CodexAppServerRecord) int {
	t.Helper()
	return len(codexRecordedSkillRoots(t, rec))
}

// codexThreadRequests are the methods that start a thread or a turn.
var codexThreadRequests = []string{"thread/start", "thread/resume", "turn/start"}

func assertNoThreadRequest(t *testing.T, rec agenttest.CodexAppServerRecord) {
	t.Helper()
	for _, m := range codexRecordedMethods(t, rec) {
		if slices.Contains(codexThreadRequests, m) {
			t.Fatalf("%s was sent although skills were not registered: %v", m, rec.Lines)
		}
	}
}

func codexSkillsRequest(t *testing.T, bin string, profileSkills ...string) (*Runner, Request) {
	t.Helper()
	req := grantAll(Request{Name: "n", Profile: codexRole("gpt-5"), Workspace: fakeWorkspace{dir: realTempDir(t)}, Prompt: "TASK"}, "FAKE_CODEX_APP_SERVER*")
	req.Profile.Skills = profileSkills
	return &Runner{CodexBin: bin, SessionsDir: t.TempDir()}, req
}

// TestCodexRegistersSkillRoots: a codex turn whose profile names skills
// prepares them, then sends exactly one skills/extraRoots/set between the
// initialized notification and thread/start, listing the absolute skills
// directory of every prepared plugin in order.
func TestCodexRegistersSkillRoots(t *testing.T) {
	bin, record := agenttest.CodexAppServer(t, agenttest.CodexAppServerScript{ThreadID: "thread-s", Actions: codexCompletedActions()})
	a, b := skillPlugin(t, "alpha"), skillPlugin(t, "beta", "gamma")
	prep := &recordingSkills{dirs: []string{a, b}}
	r, req := codexSkillsRequest(t, bin, "https://example.test/a", "https://example.test/b")
	r.Skills = prep
	res, err := r.Run(context.Background(), req)
	if err != nil || res.IsError || res.ClaudeID != "thread-s" {
		t.Fatalf("run: %+v, %v", res, err)
	}
	if prep.callCount() != 1 || !slices.Equal(prep.calls[0], []string{"https://example.test/a", "https://example.test/b"}) {
		t.Errorf("Prepare calls = %v", prep.calls)
	}
	rec := agenttest.ReadCodexAppServerRecord(t, record)
	want := []string{"initialize", "initialized", "skills/extraRoots/set", "thread/start", "turn/start"}
	if got := codexRecordedMethods(t, rec); !slices.Equal(got, want) {
		t.Fatalf("conversation = %v, want %v", got, want)
	}
	roots := codexRecordedSkillRoots(t, rec)
	if len(roots) != 1 || !slices.Equal(roots[0], []string{filepath.Join(a, "skills"), filepath.Join(b, "skills")}) {
		t.Fatalf("extraRoots = %v, want the skills directory of each prepared plugin", roots)
	}
	for _, root := range roots[0] {
		if !filepath.IsAbs(root) {
			t.Errorf("root %q is not absolute", root)
		}
	}
	for _, line := range rec.Lines {
		if strings.Contains(line, "perCwdExtraUserRoots") {
			t.Errorf("registered through perCwdExtraUserRoots: %s", line)
		}
	}
}

// TestCodexSkillRootsCoverEveryTurnKind: a held turn registers its skills
// ahead of config/read, and a resumed turn ahead of thread/resume.
func TestCodexSkillRootsCoverEveryTurnKind(t *testing.T) {
	t.Run("held", func(t *testing.T) {
		config, origins := codexConfigAnswer([]string{"apply_patch"})
		bin, record := agenttest.CodexAppServer(t, agenttest.CodexAppServerScript{
			ConfigResult: map[string]any{"config": config, "origins": origins},
			ThreadID:     "thread-g",
			Actions:      codexCompletedActions(),
		})
		plugin := skillPlugin(t, "alpha")
		r, req := codexHeldPlacements()[0].setup(t, bin)
		r.Skills = &recordingSkills{dirs: []string{plugin}}
		req.Profile.Skills = []string{"ref"}
		res, err := r.Run(context.Background(), req)
		if err != nil || res.IsError || res.ClaudeID != "thread-g" {
			t.Fatalf("run: %+v, %v", res, err)
		}
		rec := agenttest.ReadCodexAppServerRecord(t, record)
		want := []string{"initialize", "initialized", "skills/extraRoots/set", "config/read", "thread/start", "turn/start"}
		if got := codexRecordedMethods(t, rec); !slices.Equal(got, want) {
			t.Fatalf("conversation = %v, want %v", got, want)
		}
		if roots := codexRecordedSkillRoots(t, rec); len(roots) != 1 || !slices.Equal(roots[0], []string{filepath.Join(plugin, "skills")}) {
			t.Fatalf("extraRoots = %v", roots)
		}
	})
	t.Run("resumed", func(t *testing.T) {
		bin, record := agenttest.CodexAppServer(t, agenttest.CodexAppServerScript{ResumeID: "thread-r", Actions: codexCompletedActions()})
		plugin := skillPlugin(t, "alpha")
		r, req := codexSkillsRequest(t, bin, "ref")
		r.Skills = &recordingSkills{dirs: []string{plugin}}
		req.ResumeID = "earlier-thread"
		res, err := r.Run(context.Background(), req)
		if err != nil || res.IsError || res.ClaudeID != "thread-r" {
			t.Fatalf("run: %+v, %v", res, err)
		}
		rec := agenttest.ReadCodexAppServerRecord(t, record)
		want := []string{"initialize", "initialized", "skills/extraRoots/set", "thread/resume", "turn/start"}
		if got := codexRecordedMethods(t, rec); !slices.Equal(got, want) {
			t.Fatalf("conversation = %v, want %v", got, want)
		}
	})
}

// TestCodexWithoutSkillsSendsNoRegistration: a profile without skills never
// calls the preparer and has no skills/extraRoots/set line, and its
// app-server command line matches a turn that names skills but for the
// session directory.
func TestCodexWithoutSkillsSendsNoRegistration(t *testing.T) {
	run := func(t *testing.T, skills []string) (agenttest.CodexAppServerRecord, *recordingSkills) {
		t.Helper()
		bin, record := agenttest.CodexAppServer(t, agenttest.CodexAppServerScript{Actions: codexCompletedActions()})
		prep := &recordingSkills{dirs: []string{skillPlugin(t, "alpha")}}
		r, req := codexSkillsRequest(t, bin, skills...)
		r.Skills = prep
		res, err := r.Run(context.Background(), req)
		if err != nil || res.IsError {
			t.Fatalf("run: %+v, %v", res, err)
		}
		return agenttest.ReadCodexAppServerRecord(t, record), prep
	}
	plain, prep := run(t, nil)
	if prep.callCount() != 0 {
		t.Errorf("Prepare called %d times for a profile without skills", prep.callCount())
	}
	want := []string{"initialize", "initialized", "thread/start", "turn/start"}
	if got := codexRecordedMethods(t, plain); !slices.Equal(got, want) {
		t.Fatalf("conversation = %v, want %v", got, want)
	}
	withSkills, _ := run(t, []string{"ref"})
	if got, want := len(withSkills.Argv[0]), len(plain.Argv[0]); got != want {
		t.Errorf("a turn with skills has %d arguments, one without has %d:\n%v\n%v", got, want, withSkills.Argv[0], plain.Argv[0])
	}
	for _, arg := range plain.Argv[0] {
		if strings.Contains(arg, "skills") {
			t.Errorf("argument %q mentions skills", arg)
		}
	}
}

// TestCodexSkillRegistrationFailures: an error answer (method not found
// included) and a server that ends first fail the session before any thread
// starts, with a reason naming the request.
func TestCodexSkillRegistrationFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script agenttest.CodexAppServerScript
		want   []string
	}{
		{"method not found", agenttest.CodexAppServerScript{SkillsError: &agenttest.CodexRPCError{Code: -32601, Message: "Method not found: skills/extraRoots/set"}},
			[]string{"skills/extraRoots/set", "Method not found"}},
		{"invalid params", agenttest.CodexAppServerScript{SkillsError: &agenttest.CodexRPCError{Code: -32602, Message: "extraRoots must be absolute"}},
			[]string{"skills/extraRoots/set", "extraRoots must be absolute"}},
		{"server exits", agenttest.CodexAppServerScript{ExitBefore: "skills/extraRoots/set"},
			[]string{"skills/extraRoots/set"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.script.ThreadID = "thread-s"
			tc.script.Actions = codexCompletedActions()
			bin, record := agenttest.CodexAppServer(t, tc.script)
			r, req := codexSkillsRequest(t, bin, "ref")
			r.Skills = &recordingSkills{dirs: []string{skillPlugin(t, "alpha")}}
			res, err := r.Run(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if !res.IsError || res.ErrorSubtype != "error" {
				t.Fatalf("result = %+v, want an error", res)
			}
			for _, want := range append(tc.want, "skills") {
				if !strings.Contains(res.ResultText, want) {
					t.Errorf("result text %q lacks %q", res.ResultText, want)
				}
			}
			rec := agenttest.ReadCodexAppServerRecord(t, record)
			if codexSkillRequestCount(t, rec) != 1 {
				t.Errorf("skills/extraRoots/set requests: %v", codexRecordedMethods(t, rec))
			}
			assertNoThreadRequest(t, rec)
		})
	}
}

// TestCodexSkillSetupFailuresStopBeforeTheServerStarts: no preparer, a
// failed preparation, nothing prepared and a prepared plugin without a
// skills directory each end the session with an error naming skills, and
// the app-server never starts.
func TestCodexSkillSetupFailuresStopBeforeTheServerStarts(t *testing.T) {
	prepareErr := errors.New("clone failed")
	noSkillsDir := realTempDir(t)
	for _, tc := range []struct {
		name  string
		skill SkillPreparer
		want  string
	}{
		{"no preparer", nil, "skills configured but no skills manager"},
		{"preparation fails", &recordingSkills{err: prepareErr}, "prepare skills: clone failed"},
		{"nothing prepared", &recordingSkills{}, "none were prepared"},
		{"no skills directory", &recordingSkills{dirs: []string{noSkillsDir}}, "has no skills directory"},
		{"skills is a file", &recordingSkills{dirs: []string{fileSkills(t)}}, "has no skills directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, record := agenttest.CodexAppServer(t, agenttest.CodexAppServerScript{Actions: codexCompletedActions()})
			r, req := codexSkillsRequest(t, bin, "ref")
			r.Skills = tc.skill
			res, err := r.Run(context.Background(), req)
			if err == nil {
				t.Fatalf("run succeeded: %+v", res)
			}
			if !strings.Contains(err.Error(), "skills") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one naming skills and %q", err, tc.want)
			}
			if n := codexRecordStarts(t, record); n != 0 {
				t.Errorf("the fake started %d times", n)
			}
		})
	}
}

// fileSkills is a plugin directory whose skills entry is a regular file.
func fileSkills(t *testing.T) string {
	t.Helper()
	dir := realTempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "skills"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestCodexSkillRootsFollowSymlinkedWrappers: the layout skills.Manager
// builds, skills/<name> or skills itself a symlink, registers the wrapper's
// own skills path, which resolves to the SKILL.md on disk.
func TestCodexSkillRootsFollowSymlinkedWrappers(t *testing.T) {
	target := realTempDir(t)
	if err := os.WriteFile(filepath.Join(target, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	wrapper := realTempDir(t)
	if err := os.MkdirAll(filepath.Join(wrapper, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(wrapper, "skills", "one")); err != nil {
		t.Fatal(err)
	}
	roots, err := codexSkillRoots(context.Background(), &Runner{Skills: &recordingSkills{dirs: []string{wrapper}}}, []string{"ref"})
	if err != nil || !slices.Equal(roots, []string{filepath.Join(wrapper, "skills")}) {
		t.Fatalf("roots = %v, %v", roots, err)
	}
	if _, err := os.Stat(filepath.Join(roots[0], "one", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
}
