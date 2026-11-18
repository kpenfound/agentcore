package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/agentcore/vcs"
)

const (
	// DefaultCompletionTimeout bounds a completion whose request names no
	// timeout: the whole call, sandbox startup included.
	DefaultCompletionTimeout = 5 * time.Minute
	// DefaultCompletionMaxTurns bounds the turns of a completion whose
	// request names no limit. A turn without tools is one model call; with a
	// schema, Claude Code returns the answer through a tool of its own and
	// retries a mismatch, each of which is a turn.
	DefaultCompletionMaxTurns = 8
	// DefaultSandboxCheckTimeout bounds Runner.CheckSandbox when the runner
	// names no timeout.
	DefaultSandboxCheckTimeout = 15 * time.Second
	// MaxCompletionSystemPrompt is the longest system prompt a completion
	// takes, in bytes. It travels as one command-line argument, which the
	// operating system limits.
	MaxCompletionSystemPrompt = 100 * 1024

	// EnvMaxOutputTokens is the variable Claude Code reads the most output
	// tokens of a response from.
	EnvMaxOutputTokens = "CLAUDE_CODE_MAX_OUTPUT_TOKENS"
)

// CompletionRequest is one tool-free turn of claude in a Docker Sandbox.
type CompletionRequest struct {
	// Name identifies the call in logs and errors, and names its sandbox.
	// Empty is "completion".
	Name string
	// Model is passed to claude as given. Empty leaves it to claude.
	Model string
	// SystemPrompt replaces claude's default system prompt. It is required,
	// and at most MaxCompletionSystemPrompt bytes.
	SystemPrompt string
	// Prompt is the one user message. A caller with a conversation flattens
	// it into this text.
	Prompt string
	// Schema is a JSON Schema the answer must match, passed with
	// --json-schema. Empty asks for free text.
	Schema json.RawMessage
	// MaxOutputTokens, when above zero, is passed to claude as
	// CLAUDE_CODE_MAX_OUTPUT_TOKENS. Zero passes nothing.
	MaxOutputTokens int
	// MaxTurns bounds claude's turns (--max-turns). Zero is
	// DefaultCompletionMaxTurns.
	MaxTurns int
	// Timeout is the deadline of the whole call: the sandbox starting and the
	// turn together. Zero is DefaultCompletionTimeout. The caller's context
	// applies from the start of the call besides.
	Timeout time.Duration
	// CostCapUSD, when above zero, is passed to claude as --max-budget-usd.
	CostCapUSD float64
	// SandboxTemplate, SandboxCPUs and SandboxMemory are the template image
	// and the sizing the sandbox is created with (Profile.SandboxImage,
	// Profile.SandboxCPUs, Profile.SandboxMemory). Zero values leave them to
	// sbx.
	SandboxTemplate string
	SandboxCPUs     int
	SandboxMemory   string
}

// Completion is what a completion's turn reported.
type Completion struct {
	// Text is the turn's final text.
	Text string
	// StructuredOutput is the schema-checked answer claude reported, as raw
	// JSON; nil when the request had no schema.
	StructuredOutput json.RawMessage
	// The token counts of the turn as claude reports them.
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
	// CostUSD is the cost claude reported, its own estimate at API prices,
	// and CostKnown whether it reported one.
	CostUSD   float64
	CostKnown bool
	// Model is the model the stream named as the one that answered; empty
	// when it named none.
	Model string
	// Duration is the turn's: from `sbx exec` starting claude to its end.
	Duration time.Duration
	// SandboxStartup is the time spent before claude began: `sbx create` and
	// what the call checked and built around it.
	SandboxStartup time.Duration
}

// UsageLimitError is a completion that ended on the account's usage or
// session limit. Find it with errors.As.
type UsageLimitError struct {
	// Session is the call's name.
	Session string
	// ResetsAt is when the limit's window rolls, as the stream reported it;
	// zero when it reported none.
	ResetsAt time.Time
	// Window is the limit's kind as the stream named it ("five_hour",
	// "seven_day"); empty when it named none.
	Window string
	// Message is the turn's result text, claude's own sentence about the
	// limit; empty when the stream carried none.
	Message string
}

func (e *UsageLimitError) Error() string {
	msg := e.Session + " session: usage limit reached"
	if e.Window != "" {
		msg += " (" + e.Window + ")"
	}
	if !e.ResetsAt.IsZero() {
		msg += ", resets " + e.ResetsAt.Format(time.RFC3339)
	}
	return msg
}

// ErrNoStructuredOutput is what a NoStructuredOutputError matches with
// errors.Is.
var ErrNoStructuredOutput = errors.New("no structured output")

// NoStructuredOutputError is a completion with a schema whose turn ended
// without structured_output and without reaching a usage limit. Find it
// with errors.As, or errors.Is(err, ErrNoStructuredOutput).
type NoStructuredOutputError struct {
	// Session is the call's name.
	Session string
	// Subtype and Result are the CLI's error subtype and result text, when
	// the turn failed (error_max_structured_output_retries, for one);
	// empty when it succeeded without structured_output.
	Subtype string
	Result  string
}

func (e *NoStructuredOutputError) Error() string {
	msg := e.Session + " session: no structured output"
	if e.Subtype != "" {
		msg += ": " + e.Subtype
	}
	if e.Result != "" {
		msg += ": " + e.Result
	}
	return msg
}

func (e *NoStructuredOutputError) Is(target error) bool { return target == ErrNoStructuredOutput }

// completionTurn is a completion's inputs to the shared lifecycle
// (runTurn) and what the lifecycle recorded for Complete.
type completionTurn struct {
	schema string
	// startup is the time before claude started, turnBegan when it did,
	// turn how long it ran, final what its stream ended with.
	startup   time.Duration
	turnBegan time.Time
	turn      time.Duration
	final     *streamEnd
}

// completionArgs is the claude command line of a completion: the caller's
// system prompt in place of claude's, no built-in tools, no setting
// sources, no skills, an empty strict MCP configuration and no session
// kept. Nothing reads a file: the sandbox is given no directory but the
// empty one the call works in. --bare is not used: it never reads the
// login the sandbox's proxy stands in for.
func completionArgs(req Request, ct *completionTurn) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-prompts", "none",
		"--system-prompt", req.SystemPrompt,
		"--tools", "",
		"--setting-sources", "",
		"--disable-slash-commands",
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
		"--no-session-persistence",
	}
	if ct.schema != "" {
		args = append(args, "--json-schema", ct.schema)
	}
	if req.Profile.Model != "" {
		args = append(args, "--model", req.Profile.Model)
	}
	args = append(args, "--max-turns", strconv.Itoa(req.Profile.MaxTurns))
	if req.CostCapUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(req.CostCapUSD, 'f', -1, 64))
	}
	return args
}

func (c CompletionRequest) validate() error {
	switch {
	case c.SystemPrompt == "":
		return errors.New("completion: SystemPrompt is required: it replaces claude's own")
	case len(c.SystemPrompt) > MaxCompletionSystemPrompt:
		return fmt.Errorf("completion: SystemPrompt is %d bytes, over the %d a command line takes", len(c.SystemPrompt), MaxCompletionSystemPrompt)
	case c.Prompt == "":
		return errors.New("completion: Prompt is required")
	case c.MaxOutputTokens < 0:
		return fmt.Errorf("completion: MaxOutputTokens %d: want a count, or 0 for none", c.MaxOutputTokens)
	case c.MaxTurns < 0:
		return fmt.Errorf("completion: MaxTurns %d: want a count, or 0 for the default", c.MaxTurns)
	case c.Timeout < 0:
		return fmt.Errorf("completion: Timeout %s: want a duration, or 0 for the default", c.Timeout)
	}
	if err := validCostCap(c.CostCapUSD); err != nil {
		return fmt.Errorf("completion: %w", err)
	}
	if len(bytes.TrimSpace(c.Schema)) > 0 {
		var schema map[string]any
		if err := json.Unmarshal(c.Schema, &schema); err != nil {
			return fmt.Errorf("completion: Schema is not a JSON object: %w", err)
		}
	}
	return nil
}

// Complete runs one claude turn in a Docker Sandbox of its own, with no
// built-in tools, no MCP server, no skills, no VCS, no Dagger engine and no
// setting source, and returns what it answered. The sandbox is created for
// the call with `sbx create`, given one empty temporary directory the call
// makes, run in with `sbx exec`, and removed with `sbx rm` however the call
// ends. No agent credential is forwarded: the sandbox's proxy supplies the
// login stored with `sbx secret set`.
//
// The request's Timeout is one deadline over the sandbox starting and the
// turn, and the context applies from the start. When either ends the call
// while `sbx create` runs, claude is never started, the sandbox that may
// have been made is removed, and the error matches the context's
// (context.DeadlineExceeded or context.Canceled).
//
// A failed call is one of four kinds, tested in this order: a
// *UsageLimitError, when the stream reported the account's usage or session
// limit, whatever a schema asked for; the context's error, for a timeout or
// a cancellation; a *NoStructuredOutputError, when the request had a schema
// and the turn ended without structured_output; any other failure, an error
// naming the session, claude's error subtype and its result text.
func (r *Runner) Complete(ctx context.Context, req CompletionRequest) (*Completion, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	name := req.Name
	if name == "" {
		name = "completion"
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultCompletionTimeout
	}
	maxTurns := req.MaxTurns
	if maxTurns == 0 {
		maxTurns = DefaultCompletionMaxTurns
	}
	// One deadline over everything below, sandbox startup included: the
	// shared lifecycle starts the sandbox before it applies the profile's
	// own timeout, so that one is not enough.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	runner := *r
	runner.completing = true
	runner.AddDirs, runner.MountDirs, runner.SkillMountDirs, runner.Skills = nil, nil, nil, nil
	if runner.SessionsDir == "" {
		runner.SessionsDir = os.TempDir()
	}
	if runner.Logger == nil {
		runner.Logger = slog.Default()
	}
	sessionDir, err := runner.NewSessionDir(name)
	if err != nil {
		return nil, fmt.Errorf("%s session: %w", name, err)
	}
	defer func() { _ = os.RemoveAll(sessionDir) }()
	work, err := os.MkdirTemp("", runner.namePrefix()+"completion-")
	if err != nil {
		return nil, fmt.Errorf("%s session: make the working directory: %w", name, err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	if resolved, err := filepath.EvalSymlinks(work); err == nil {
		work = resolved
	}

	profile := Profile{
		Name: name, Agent: AgentClaude, Model: req.Model, MaxTurns: maxTurns, Timeout: timeout,
		Sandbox: SandboxSbx, SandboxImage: req.SandboxTemplate, SandboxCPUs: req.SandboxCPUs, SandboxMemory: req.SandboxMemory,
	}
	grants := &Grants{Tools: []string{}, Mounts: []Mount{{Path: work, Access: ReadWrite}}}
	if req.MaxOutputTokens > 0 {
		profile.Env = map[string]string{EnvMaxOutputTokens: strconv.Itoa(req.MaxOutputTokens)}
		grants.Env = []string{EnvMaxOutputTokens}
	}
	turn := &completionTurn{}
	if len(bytes.TrimSpace(req.Schema)) > 0 {
		turn.schema = string(req.Schema)
	}
	res, err := runner.runTurn(ctx, Request{
		Name: name, Profile: profile, Workspace: vcs.Directory(work),
		SystemPrompt: req.SystemPrompt, Prompt: req.Prompt, SessionDir: sessionDir,
		CostCapUSD: req.CostCapUSD, Grants: grants,
	}, turnKind{completion: turn})
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s session: %w: %v", name, ctx.Err(), err)
		}
		return nil, fmt.Errorf("%s session: %w", name, err)
	}
	return classifyCompletion(name, timeout, turn, res)
}

// classifyCompletion turns a finished turn into the completion or the one
// kind of error it is, in the order Complete documents.
func classifyCompletion(name string, timeout time.Duration, turn *completionTurn, res *Result) (*Completion, error) {
	if resets, limited := res.SessionLimited(); limited {
		e := &UsageLimitError{Session: name, ResetsAt: resets, Message: strings.TrimSpace(res.ResultText)}
		if res.RateLimit != nil {
			e.Window = res.RateLimit.Type
		}
		return nil, e
	}
	if res.TimedOut {
		return nil, fmt.Errorf("%s session: timed out after %s: %w", name, timeout, context.DeadlineExceeded)
	}
	failed := res.IsError || res.ExitCode != 0
	var structured json.RawMessage
	var usage tokenUsage
	var model string
	costKnown := res.CostKnown
	if turn.final != nil {
		usage, model = turn.final.Usage, turn.final.Model
		costKnown = costKnown && turn.final.CostReported
		if so := bytes.TrimSpace(turn.final.StructuredOutput); len(so) > 0 && string(so) != "null" {
			structured = append(json.RawMessage(nil), so...)
		}
	}
	if turn.schema != "" && structured == nil {
		e := &NoStructuredOutputError{Session: name}
		if failed {
			e.Subtype, e.Result = res.ErrorSubtype, strings.TrimSpace(res.ResultText)
			if e.Subtype == "" {
				e.Subtype = "failed"
			}
		}
		return nil, e
	}
	if failed {
		return nil, restrictedFailure(name, res)
	}
	if turn.schema == "" {
		structured = nil
	}
	return &Completion{
		Text:                res.ResultText,
		StructuredOutput:    structured,
		InputTokens:         usage.Input,
		OutputTokens:        usage.Output,
		CacheCreationTokens: usage.CacheCreation,
		CacheReadTokens:     usage.CacheRead,
		CostUSD:             res.CostUSD,
		CostKnown:           costKnown,
		Model:               model,
		Duration:            turn.turn,
		SandboxStartup:      turn.startup,
	}, nil
}

// CheckSandbox reports whether Docker Sandboxes can be used: it runs one
// cheap `sbx ls` under a short timeout (Runner.SbxCheckTimeout, else
// DefaultSandboxCheckTimeout), which needs the sbx CLI installed and its
// daemon answering. It creates no sandbox and makes no model call. A failure
// names the sbx executable and carries sbx's own message, or says the
// command did not answer in time.
func (r *Runner) CheckSandbox(ctx context.Context) error {
	timeout := r.SbxCheckTimeout
	if timeout <= 0 {
		timeout = DefaultSandboxCheckTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	runner := *r
	runner.completing = true
	cmd := runner.sbxCommand(ctx, "ls")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	bin := r.sbxBin()
	if ctx.Err() != nil {
		err = fmt.Errorf("no answer within %s: %w", timeout, ctx.Err())
	} else if msg := bytes.TrimSpace(out); len(msg) > 0 {
		err = fmt.Errorf("%w: %s", err, msg)
	}
	return fmt.Errorf("%s is not available (%s ls): %w", bin, bin, err)
}

// hostEnv is the environment the runner's own commands (the sbx client
// among them) run in: the host's, less the variables of EnvironmentPrefix.
// For a completion it is also less every agent credential and the variable
// a completion sets itself, so that nothing the caller's environment holds
// is in the client's, and the request alone decides what the sandboxed
// claude is handed.
func (r *Runner) hostEnv() []string {
	env := hostEnv(r.EnvironmentPrefix)
	if !r.completing {
		return env
	}
	return slices.DeleteFunc(env, func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return completionScrubbed(name)
	})
}

// completionScrubbed says whether a host variable is kept from a
// completion's commands: a credential of any agent, anything Anthropic's or
// Claude Code's login is read from, and the output token limit.
func completionScrubbed(name string) bool {
	if name == EnvMaxOutputTokens || strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "CLAUDE_CODE_OAUTH") {
		return true
	}
	for _, b := range Backends {
		if slices.Contains(b.Credentials, name) {
			return true
		}
	}
	return false
}
