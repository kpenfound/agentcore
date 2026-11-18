package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent/agenttest"
	"github.com/kpenfound/agentcore/vcs"
)

// recordClaude is a fake claude that records what it was started with beside
// the sbx recordings, then prints the stream it is given.
const recordClaude = `
printf '%s\n' "$@" > "$COMPLETION_REC/claude-args.txt"
env > "$COMPLETION_REC/claude-env.txt"
pwd > "$COMPLETION_REC/claude-pwd.txt"
ls -A | wc -l > "$COMPLETION_REC/claude-files.txt"
cat > "$COMPLETION_REC/claude-stdin.txt"
touch "$COMPLETION_REC/claude-started"
`

// completionFixture is a runner whose sbx and claude are fakes, with the
// temporary directory the call makes its working directory in pointed at a
// directory of the test's.
type completionFixture struct {
	r *Runner
	// rec holds the recordings of the fake claude; sbx the fake sbx's, beside
	// its script; tmp is where the call's directories are made.
	rec, sbx, tmp string
}

func newCompletionFixture(t *testing.T, claudeBody string) *completionFixture {
	t.Helper()
	// The fakes must lie under the temporary directory (agentbin), so it
	// is the test's own root, and what the call leaves in it is what the
	// test did not put there.
	tmp := filepath.Dir(t.TempDir())
	t.Setenv("TMPDIR", tmp)
	rec := t.TempDir()
	t.Setenv("COMPLETION_REC", rec)
	sbx := agenttest.Sbx(t, "COMPLETION_REC")
	claude := agenttest.Script(t, "claude", claudeBody)
	return &completionFixture{
		r:   &Runner{ClaudeBin: claude, SbxBin: sbx, SessionsDir: t.TempDir(), NamePrefix: "task-"},
		rec: rec, sbx: filepath.Dir(sbx), tmp: tmp,
	}
}

func (f *completionFixture) mark(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.sbx, name), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// entries are the names in the call's temporary directory and the runner's
// sessions directory: what a call must leave as it found.
func (f *completionFixture) entries(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, dir := range []string{f.tmp, f.r.SessionsDir} {
		list, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range list {
			names = append(names, filepath.Join(dir, e.Name()))
		}
	}
	slices.Sort(names)
	return names
}

func (f *completionFixture) recorded(t *testing.T, name string) []string {
	t.Helper()
	return lines(t, filepath.Join(f.rec, name))
}

func (f *completionFixture) sbxRecorded(t *testing.T, name string) []string {
	t.Helper()
	return lines(t, filepath.Join(f.sbx, name))
}

func (f *completionFixture) exists(name string) bool {
	_, err := os.Stat(filepath.Join(f.sbx, name))
	if err != nil {
		_, err = os.Stat(filepath.Join(f.rec, name))
	}
	return err == nil
}

// created is what `sbx create` was given: its sandbox's name and workspaces.
func (f *completionFixture) created(t *testing.T) (name string, workspaces []string) {
	t.Helper()
	args := f.sbxRecorded(t, "sbx-create.txt")
	i := slices.Index(args, "claude")
	if i < 0 || slices.Index(args, "--name") < 0 {
		t.Fatalf("sbx create args: %v", args)
	}
	return args[slices.Index(args, "--name")+1], args[i+1:]
}

func completionRequest() CompletionRequest {
	return CompletionRequest{Name: "distill_1", Model: "claude-test", SystemPrompt: "You are a distiller.", Prompt: "Summarise this.", Timeout: time.Minute}
}

const okStream = `
echo '{"type":"system","subtype":"init","model":"claude-init"}'
echo '{"type":"assistant","message":{"model":"claude-answered"}}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"the answer","session_id":"s1","num_turns":1,"total_cost_usd":0.0123,"usage":{"input_tokens":11,"output_tokens":22,"cache_creation_input_tokens":33,"cache_read_input_tokens":44}}'
`

// A completion is `sbx create`, `sbx exec` of claude -p, and `sbx rm` of the
// same sandbox, in a directory of its own that is empty while claude runs and
// gone afterwards. Claude is given the caller's system prompt in place of its
// own, no tools, no setting sources and an empty strict MCP configuration, the
// prompt on stdin, and none of the caller's credentials.
func TestCompleteRunsClaudeInASandbox(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-host-secret")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-host-secret")
	f := newCompletionFixture(t, recordClaude+okStream)
	before := f.entries(t)

	c, err := f.r.Complete(context.Background(), completionRequest())
	if err != nil {
		t.Fatal(err)
	}

	if got, want := f.sbxRecorded(t, "sbx-calls.txt"), []string{"create --no-progress", "rm --force"}; !slices.Equal(got, want) {
		t.Errorf("sbx calls: %v, want %v", got, want)
	}
	name, workspaces := f.created(t)
	if rm := f.sbxRecorded(t, "sbx-rm.txt"); !slices.Equal(rm, []string{"rm", "--force", name}) {
		t.Errorf("sbx rm: %v, want the removal of %s", rm, name)
	}
	execArgs := f.recorded(t, "sbx-exec-args.txt")
	if execArgs[0] != "exec" || !slices.Contains(execArgs, name) {
		t.Fatalf("sbx exec args: %v", execArgs)
	}
	if i := slices.Index(execArgs, name); execArgs[i+1] != f.r.ClaudeBin || execArgs[i+2] != "-p" {
		t.Errorf("sbx exec runs %v, want claude -p", execArgs[i+1:])
	}

	// The sandbox is given the call's directory and nothing else; it was
	// empty when claude ran in it and is gone.
	if len(workspaces) != 1 || strings.Contains(workspaces[0], ":") {
		t.Fatalf("workspaces: %v, want the call's one read-write directory", workspaces)
	}
	if pwd := f.recorded(t, "claude-pwd.txt")[0]; pwd != workspaces[0] {
		t.Errorf("claude ran in %s, want the workspace %s", pwd, workspaces[0])
	}
	if n := f.recorded(t, "claude-files.txt")[0]; strings.TrimSpace(n) != "0" {
		t.Errorf("the working directory held %s entries when claude ran", n)
	}
	if _, err := os.Stat(workspaces[0]); !os.IsNotExist(err) {
		t.Errorf("working directory %s outlived the call: %v", workspaces[0], err)
	}
	if after := f.entries(t); !slices.Equal(before, after) {
		t.Errorf("the call left %v behind", slices.DeleteFunc(slices.Clone(after), func(p string) bool { return slices.Contains(before, p) }))
	}

	args := f.recorded(t, "claude-args.txt")
	arg := func(flag string) string {
		t.Helper()
		i := slices.Index(args, flag)
		if i < 0 || i+1 >= len(args) {
			t.Fatalf("claude args lack %s: %q", flag, args)
		}
		return args[i+1]
	}
	for _, flag := range []string{"-p", "--strict-mcp-config", "--no-session-persistence"} {
		if !slices.Contains(args, flag) {
			t.Errorf("claude args lack %s: %q", flag, args)
		}
	}
	if got := arg("--system-prompt"); got != "You are a distiller." {
		t.Errorf("--system-prompt %q", got)
	}
	for _, bad := range []string{"--append-system-prompt", "--append-system-prompt-file", "--bare", "--json-schema", "--dangerously-skip-permissions", "--add-dir", "--allowedTools"} {
		if slices.Contains(args, bad) {
			t.Errorf("claude args carry %s: %q", bad, args)
		}
	}
	if got := arg("--tools"); got != "" {
		t.Errorf("--tools %q, want none", got)
	}
	if got := arg("--setting-sources"); got != "" {
		t.Errorf("--setting-sources %q, want none", got)
	}
	if got := arg("--mcp-config"); got != `{"mcpServers":{}}` {
		t.Errorf("--mcp-config %q, want an empty one", got)
	}
	if got := arg("--model"); got != "claude-test" {
		t.Errorf("--model %q", got)
	}
	if got := arg("--max-turns"); got != "8" {
		t.Errorf("--max-turns %q", got)
	}
	if got := strings.Join(f.recorded(t, "claude-stdin.txt"), "\n"); got != "Summarise this." {
		t.Errorf("claude's stdin %q, want the prompt", got)
	}

	// Neither credential reaches sbx exec's environment or arguments.
	env := strings.Join(f.recorded(t, "sbx-exec-env.txt"), "\n")
	for _, secret := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "sk-host-secret", "oauth-host-secret"} {
		if strings.Contains(env, secret) || strings.Contains(strings.Join(execArgs, " "), secret) {
			t.Errorf("sbx exec carries %s", secret)
		}
	}
	if !strings.Contains(strings.Join(execArgs, " "), "--workdir "+workspaces[0]) {
		t.Errorf("sbx exec args lack the workdir: %v", execArgs)
	}
	if slices.Contains(execArgs, "--env") {
		t.Errorf("sbx exec passes variables though none were asked for: %v", execArgs)
	}

	if c.Text != "the answer" || c.StructuredOutput != nil {
		t.Errorf("completion %+v", c)
	}
}

// What the result event reports is what the completion carries.
func TestCompleteReportsUsageCostModelAndStartup(t *testing.T) {
	f := newCompletionFixture(t, okStream)
	c, err := f.r.Complete(context.Background(), completionRequest())
	if err != nil {
		t.Fatal(err)
	}
	if c.InputTokens != 11 || c.OutputTokens != 22 || c.CacheCreationTokens != 33 || c.CacheReadTokens != 44 {
		t.Errorf("tokens: %+v", c)
	}
	if !c.CostKnown || c.CostUSD != 0.0123 {
		t.Errorf("cost: %v %v", c.CostUSD, c.CostKnown)
	}
	if c.Model != "claude-answered" {
		t.Errorf("model %q, want the one that answered", c.Model)
	}
	if c.SandboxStartup <= 0 || c.Duration <= 0 {
		t.Errorf("startup %s, duration %s", c.SandboxStartup, c.Duration)
	}
}

func TestCompleteReportsNoCostWhenNoneWasReported(t *testing.T) {
	f := newCompletionFixture(t, `echo '{"type":"result","subtype":"success","is_error":false,"result":"x"}'`)
	c, err := f.r.Complete(context.Background(), completionRequest())
	if err != nil {
		t.Fatal(err)
	}
	if c.CostKnown || c.CostUSD != 0 || c.Model != "" {
		t.Errorf("completion %+v", c)
	}
}

// The sandbox is removed however the turn ends.
func TestCompleteRemovesTheSandboxWhenTheTurnFails(t *testing.T) {
	f := newCompletionFixture(t, recordClaude+`
echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"it broke"}'
exit 1
`)
	_, err := f.r.Complete(context.Background(), completionRequest())
	if err == nil {
		t.Fatal("a failed turn succeeded")
	}
	f.requireRemoved(t)
}

func (f *completionFixture) requireRemoved(t *testing.T) {
	t.Helper()
	name, workspaces := f.created(t)
	if rm := f.sbxRecorded(t, "sbx-rm.txt"); !slices.Equal(rm, []string{"rm", "--force", name}) {
		t.Errorf("sbx rm: %v, want one removal of %s", rm, name)
	}
	if _, err := os.Stat(workspaces[0]); !os.IsNotExist(err) {
		t.Errorf("working directory %s outlived the call", workspaces[0])
	}
}

func TestCompleteRemovesTheSandboxWhenTheTurnTimesOut(t *testing.T) {
	f := newCompletionFixture(t, recordClaude+`sleep 30`)
	req := completionRequest()
	req.Timeout = time.Second
	start := time.Now()
	_, err := f.r.Complete(context.Background(), req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v, want a deadline", err)
	}
	if took := time.Since(start); took > 15*time.Second {
		t.Errorf("took %s for a 1s timeout", took)
	}
	f.requireRemoved(t)
}

func TestCompleteRemovesTheSandboxWhenCancelledWhileClaudeRuns(t *testing.T) {
	f := newCompletionFixture(t, recordClaude+`sleep 30`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for !f.exists("claude-started") {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	_, err := f.r.Complete(ctx, completionRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v, want a cancellation", err)
	}
	f.requireRemoved(t)
}

// The timeout covers `sbx create`: a create that never finishes ends at the
// deadline with its context error, claude is never started, removal of the
// sandbox is still attempted, and nothing is left in the temporary directory.
func TestCompleteTimeoutBoundsSandboxStartup(t *testing.T) {
	f := newCompletionFixture(t, recordClaude+okStream)
	f.mark(t, "hang-create")
	before := f.entries(t)
	req := completionRequest()
	req.Timeout = 500 * time.Millisecond
	start := time.Now()
	_, err := f.r.Complete(context.Background(), req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v, want a deadline", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("took %s for a 500ms timeout", took)
	}
	f.requireStartupEndedClean(t, before)
}

func TestCompleteCancellationBoundsSandboxStartup(t *testing.T) {
	f := newCompletionFixture(t, recordClaude+okStream)
	f.mark(t, "hang-create")
	before := f.entries(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for !f.exists("sbx-create.txt") {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	_, err := f.r.Complete(ctx, completionRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v, want a cancellation", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("took %s to notice the cancellation", took)
	}
	f.requireStartupEndedClean(t, before)
}

func (f *completionFixture) requireStartupEndedClean(t *testing.T, before []string) {
	t.Helper()
	name, workspaces := f.created(t)
	if f.exists("sbx-exec-args.txt") || f.exists("claude-started") {
		t.Error("claude was started although the sandbox never was")
	}
	if rm := f.sbxRecorded(t, "sbx-rm.txt"); !slices.Equal(rm, []string{"rm", "--force", name}) {
		t.Errorf("sbx rm: %v, want an attempt to remove %s", rm, name)
	}
	if _, err := os.Stat(workspaces[0]); !os.IsNotExist(err) {
		t.Errorf("working directory %s outlived the call", workspaces[0])
	}
	if after := f.entries(t); !slices.Equal(before, after) {
		t.Errorf("entries %v, want %v", after, before)
	}
}

const schema = `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`

func TestCompleteReturnsStructuredOutput(t *testing.T) {
	f := newCompletionFixture(t, recordClaude+`
echo '{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":{"n": 7},"total_cost_usd":0.5,"usage":{"input_tokens":1,"output_tokens":2}}'
`)
	req := completionRequest()
	req.Schema = []byte(schema)
	c, err := f.r.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if string(c.StructuredOutput) != `{"n": 7}` {
		t.Errorf("structured output %q", c.StructuredOutput)
	}
	args := f.recorded(t, "claude-args.txt")
	if i := slices.Index(args, "--json-schema"); i < 0 || args[i+1] != schema {
		t.Errorf("--json-schema: %q", args)
	}
}

func TestCompleteWithoutSchemaDoesNotReturnStructuredOutput(t *testing.T) {
	f := newCompletionFixture(t, `echo '{"type":"result","subtype":"success","is_error":false,"result":"x","structured_output":{"n":1}}'`)
	c, err := f.r.Complete(context.Background(), completionRequest())
	if err != nil {
		t.Fatal(err)
	}
	if c.StructuredOutput != nil {
		t.Errorf("structured output %q without a schema", c.StructuredOutput)
	}
}

func TestCompleteReportsMissingStructuredOutput(t *testing.T) {
	for _, tc := range []struct {
		name, stream, subtype, result string
	}{
		{"success", `{"type":"result","subtype":"success","is_error":false,"result":"free text instead"}`, "", ""},
		{"null", `{"type":"result","subtype":"success","is_error":false,"result":"free text instead","structured_output":null}`, "", ""},
		{"retries", `{"type":"result","subtype":"error_max_structured_output_retries","is_error":true,"result":"the schema was never met"}`, "error_max_structured_output_retries", "the schema was never met"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCompletionFixture(t, "echo '"+tc.stream+"'\n")
			req := completionRequest()
			req.Schema = []byte(schema)
			_, err := f.r.Complete(context.Background(), req)
			var missing *NoStructuredOutputError
			if !errors.Is(err, ErrNoStructuredOutput) || !errors.As(err, &missing) {
				t.Fatalf("error %v, want the missing structured output error", err)
			}
			if missing.Subtype != tc.subtype || missing.Result != tc.result {
				t.Errorf("error fields %+v, want subtype %q and result %q", missing, tc.subtype, tc.result)
			}
			if tc.subtype != "" {
				for _, want := range []string{"distill_1", tc.subtype, tc.result} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q lacks %q", err, want)
					}
				}
			}
			var limit *UsageLimitError
			if errors.As(err, &limit) {
				t.Error("a missing structured output is a usage limit")
			}
			f.requireRemoved(t)
		})
	}
}

// A usage or session limit is its own error with the reset time, before
// anything else, a missing structured output included.
func TestCompleteReportsTheUsageLimit(t *testing.T) {
	reset := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	blocked := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","resetsAt":` + strconv.FormatInt(reset.Unix(), 10) + `}}`
	limitResult := `{"type":"result","subtype":"success","is_error":true,"result":"You've hit your session limit · resets 11:50pm"}`
	for _, tc := range []struct {
		name, stream string
		schema       bool
		resets       time.Time
	}{
		{"blocking event", blocked + "\n" + `{"type":"result","subtype":"success","is_error":false,"result":"partial"}`, false, reset},
		{"blocking event with a schema and no structured output", blocked + "\n" + `{"type":"result","subtype":"error_max_structured_output_retries","is_error":true,"result":"gave up"}`, true, reset},
		{"result text", limitResult, false, time.Time{}},
		{"result text with a schema", limitResult, true, time.Time{}},
		{"usage limit text", `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Claude usage limit reached"}`, false, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCompletionFixture(t, "cat <<'EOF'\n"+tc.stream+"\nEOF\n")
			req := completionRequest()
			if tc.schema {
				req.Schema = []byte(schema)
			}
			_, err := f.r.Complete(context.Background(), req)
			var limit *UsageLimitError
			if !errors.As(err, &limit) {
				t.Fatalf("error %v, want a usage limit", err)
			}
			if !limit.ResetsAt.Equal(tc.resets) {
				t.Errorf("resets at %v, want %v", limit.ResetsAt, tc.resets)
			}
			if limit.Session != "distill_1" {
				t.Errorf("session %q", limit.Session)
			}
			if errors.Is(err, ErrNoStructuredOutput) {
				t.Error("a usage limit is a missing structured output")
			}
			f.requireRemoved(t)
		})
	}
}

// Any other failure names the session, claude's subtype and its text.
func TestCompleteReportsOtherFailures(t *testing.T) {
	f := newCompletionFixture(t, `
echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"the model fell over"}'
exit 1
`)
	_, err := f.r.Complete(context.Background(), completionRequest())
	if err == nil {
		t.Fatal("a failed turn succeeded")
	}
	for _, want := range []string{"distill_1", "error_during_execution", "the model fell over"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	var limit *UsageLimitError
	if errors.As(err, &limit) || errors.Is(err, ErrNoStructuredOutput) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v is another kind", err)
	}
}

func TestCompleteReportsAStreamWithNoResult(t *testing.T) {
	f := newCompletionFixture(t, `echo "claude: not logged in" >&2; exit 1`)
	_, err := f.r.Complete(context.Background(), completionRequest())
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("error %v, want claude's stderr", err)
	}
	f.requireRemoved(t)
}

// A maximum output token count reaches the sandboxed claude, by name on the
// sbx command line and as a value in its environment; none is passed when it
// is unset, whatever the caller's own environment holds.
func TestCompleteMaxOutputTokens(t *testing.T) {
	t.Setenv("CLAUDE_CODE_MAX_OUTPUT_TOKENS", "999")
	f := newCompletionFixture(t, recordClaude+okStream)
	req := completionRequest()
	req.MaxOutputTokens = 4096
	if _, err := f.r.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if env := f.recorded(t, "claude-env.txt"); !slices.Contains(env, "CLAUDE_CODE_MAX_OUTPUT_TOKENS=4096") {
		t.Errorf("claude's environment lacks the limit: %v", env)
	}
	if args := strings.Join(f.recorded(t, "sbx-exec-args.txt"), " "); !strings.Contains(args, "--env CLAUDE_CODE_MAX_OUTPUT_TOKENS ") || strings.Contains(args, "4096") {
		t.Errorf("sbx exec args %s, want the variable by name", args)
	}

	f = newCompletionFixture(t, recordClaude+okStream)
	if _, err := f.r.Complete(context.Background(), completionRequest()); err != nil {
		t.Fatal(err)
	}
	for _, kv := range f.recorded(t, "claude-env.txt") {
		if strings.HasPrefix(kv, "CLAUDE_CODE_MAX_OUTPUT_TOKENS") {
			t.Errorf("claude's environment has %s without a request for it", kv)
		}
	}
	if args := strings.Join(f.recorded(t, "sbx-exec-args.txt"), " "); strings.Contains(args, "MAX_OUTPUT") {
		t.Errorf("sbx exec args %s name the limit", args)
	}
}

func TestCompleteSandboxSizingAndCostCap(t *testing.T) {
	f := newCompletionFixture(t, recordClaude+okStream)
	req := completionRequest()
	req.SandboxTemplate, req.SandboxCPUs, req.SandboxMemory, req.CostCapUSD, req.MaxTurns = "ghcr.io/acme/claude:1", 2, "2g", 0.25, 3
	if _, err := f.r.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	create := strings.Join(f.sbxRecorded(t, "sbx-create.txt"), " ")
	for _, want := range []string{"--template ghcr.io/acme/claude:1", "--cpus 2", "--memory 2g"} {
		if !strings.Contains(create, want) {
			t.Errorf("sbx create lacks %q: %s", want, create)
		}
	}
	args := strings.Join(f.recorded(t, "claude-args.txt"), " ")
	for _, want := range []string{"--max-budget-usd 0.25", "--max-turns 3"} {
		if !strings.Contains(args, want) {
			t.Errorf("claude args lack %q: %s", want, args)
		}
	}
}

func TestCompleteRefusesARequestItCannotRun(t *testing.T) {
	f := newCompletionFixture(t, okStream)
	for name, mutate := range map[string]func(*CompletionRequest){
		"no system prompt": func(r *CompletionRequest) { r.SystemPrompt = "" },
		"no prompt":        func(r *CompletionRequest) { r.Prompt = "" },
		"huge prompt":      func(r *CompletionRequest) { r.SystemPrompt = strings.Repeat("x", MaxCompletionSystemPrompt+1) },
		"bad schema":       func(r *CompletionRequest) { r.Schema = []byte("[1,") },
		"negative tokens":  func(r *CompletionRequest) { r.MaxOutputTokens = -1 },
		"negative timeout": func(r *CompletionRequest) { r.Timeout = -time.Second },
		"bad memory":       func(r *CompletionRequest) { r.SandboxMemory = "lots" },
		"bad cost cap":     func(r *CompletionRequest) { r.CostCapUSD = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			req := completionRequest()
			mutate(&req)
			if _, err := f.r.Complete(context.Background(), req); err == nil {
				t.Fatal("the request was run")
			}
			if f.exists("sbx-calls.txt") {
				t.Error("sbx was called")
			}
		})
	}
}

// The availability check runs `sbx ls`, creates nothing, and says which sbx
// it asked and what it answered.
func TestCheckSandbox(t *testing.T) {
	f := newCompletionFixture(t, okStream)
	if err := f.r.CheckSandbox(context.Background()); err != nil {
		t.Fatalf("a fake sbx that answers: %v", err)
	}
	if got := f.sbxRecorded(t, "sbx-ls.txt"); !slices.Equal(got, []string{"ls"}) {
		t.Errorf("sbx ls calls: %v", got)
	}

	for name, tc := range map[string]struct {
		setup func(*completionFixture)
		want  []string
	}{
		"fails": {func(f *completionFixture) { f.mark(t, "fail-ls") }, []string{"Cannot connect to the sandbox daemon"}},
		"missing": {func(f *completionFixture) {
			f.r.SbxBin = filepath.Join(f.tmp, "no-such-sbx")
		}, []string{"no-such-sbx"}},
		"hangs": {func(f *completionFixture) {
			f.mark(t, "hang-ls")
			f.r.SbxCheckTimeout = 300 * time.Millisecond
		}, []string{"no answer within 300ms"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCompletionFixture(t, okStream)
			tc.setup(f)
			start := time.Now()
			err := f.r.CheckSandbox(context.Background())
			if err == nil {
				t.Fatal("the check passed")
			}
			if took := time.Since(start); took > 10*time.Second {
				t.Errorf("took %s", took)
			}
			for _, want := range append([]string{f.r.SbxBin}, tc.want...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
			if f.exists("sbx-create.txt") || f.exists("sbx-calls.txt") {
				t.Error("the check created or removed a sandbox")
			}
		})
	}
}

// Complete and CheckSandbox change nothing for the callers that were here
// first: RunRestricted still refuses a sandbox of the caller's and a system
// prompt, Run still appends the system prompt to claude's own.
func TestCompletionLeavesRunAndRunRestrictedAlone(t *testing.T) {
	f := newCompletionFixture(t, recordClaude+okStream)
	ws := vcs.Directory(t.TempDir())
	_, err := f.r.RunRestricted(context.Background(), Request{Name: "r", Workspace: ws, Profile: Profile{Name: "r", Sandbox: SandboxSbx}})
	if err == nil || !strings.Contains(err.Error(), "caller-selected sandboxes are not supported") {
		t.Errorf("RunRestricted with a sandbox: %v", err)
	}
	_, err = f.r.RunRestricted(context.Background(), Request{Name: "r", Workspace: ws, SystemPrompt: "x", Profile: Profile{Name: "r"}})
	if err == nil || !strings.Contains(err.Error(), "SystemPrompt is not supported") {
		t.Errorf("RunRestricted with a system prompt: %v", err)
	}

	dir := t.TempDir()
	req := grantAll(Request{Name: "plain", Profile: Profile{Name: "plain", Model: "m", MaxTurns: 2, Timeout: time.Minute}, Workspace: vcs.Directory(dir), SystemPrompt: "APPENDED", Prompt: "go", SessionDir: t.TempDir(), Env: map[string]string{"COMPLETION_REC": f.rec}})
	res, err := f.r.Run(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("Run: %v %+v", err, res)
	}
	args := f.recorded(t, "claude-args.txt")
	i := slices.Index(args, "--append-system-prompt-file")
	if i < 0 || slices.Contains(args, "--system-prompt") {
		t.Fatalf("claude args %q, want the system prompt appended from a file", args)
	}
	if b, err := os.ReadFile(args[i+1]); err != nil || string(b) != "APPENDED" {
		t.Errorf("system prompt file: %q %v", b, err)
	}
}
