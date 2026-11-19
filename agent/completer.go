package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/kpenfound/agentcore/agent/procs"
	"github.com/kpenfound/agentcore/vcs"
)

// CompleterOptions configures a Completer: the sandboxes it keeps and how
// many of them may be alive at once.
type CompleterOptions struct {
	// Name names the sandboxes, after the runner's NamePrefix. Empty is
	// "completion".
	Name string
	// Size is the most sandboxes alive at once, and so the most calls that
	// run at once; a call beyond it waits for one to finish. Zero is 1.
	Size int
	// SandboxTemplate, SandboxCPUs and SandboxMemory are what each sandbox
	// is created with, as CompletionRequest's fields of the same names are
	// for Runner.Complete; a request's own are ignored.
	SandboxTemplate string
	SandboxCPUs     int
	SandboxMemory   string
}

// ErrCompleterClosed is what Completer.Complete returns once Close has been
// called.
var ErrCompleterClosed = errors.New("completer closed")

// Completer runs completions as Runner.Complete does, in Docker Sandboxes
// it keeps between calls instead of one created and removed per call. A
// sandbox is created the first time a call finds none free, and each call
// is an `sbx exec` of claude in a free one, so a steady stream of calls
// creates at most Size sandboxes: sbx resolves the agent's kit, and pulls
// its template, once per sandbox, not once per call.
//
// Each call has a working directory of its own, made empty inside the
// sandbox's workspace for the call and removed when it ends, and a session
// directory of its own on the host that the sandbox is not given. Claude is
// started exactly as for Runner.Complete: no tools to read a file or run a
// command with, no setting sources, no MCP server, and
// --no-session-persistence, so a turn writes no session for a later turn to
// resume or find, and what one call was given never reaches the next.
//
// A sandbox is kept after a call whose turn ran to the end the stream
// reports, whatever it answered: a NoStructuredOutputError, a
// UsageLimitError or a failed result is the model's outcome, not the
// sandbox's. One is removed, and the next call creates a fresh one, after a
// call that ended any other way: claude never started, the client exited
// without a result (a sandbox sbx no longer has, for one), or the call timed
// out or was cancelled while claude may still be running in it.
//
// A Completer is safe for concurrent use. Close removes its sandboxes.
type Completer struct {
	r    *Runner
	opts CompleterOptions
	// slots holds a token for each call running; its capacity is Size.
	slots chan struct{}
	// done is closed by Close, which wakes the calls waiting for a slot.
	done chan struct{}

	mu     sync.Mutex
	closed bool
	idle   []*pooledSandbox
	// calls counts the calls Complete has admitted and not yet finished,
	// which Close waits for.
	calls sync.WaitGroup
}

// pooledSandbox is one of a Completer's sandboxes: the sandbox, the
// workspace it was created with, in which each call's working directory is
// made, and the session directory that records both for orphan cleanup.
type pooledSandbox struct {
	box        *sandbox
	workspace  string
	sessionDir string
}

// NewCompleter returns a Completer that runs completions with this runner's
// executables, sessions directory and name prefix. It creates nothing:
// the first call creates the first sandbox.
func (r *Runner) NewCompleter(opts CompleterOptions) *Completer {
	if opts.Name == "" {
		opts.Name = "completion"
	}
	if opts.Size <= 0 {
		opts.Size = 1
	}
	return &Completer{r: r.completionRunner(), opts: opts, slots: make(chan struct{}, opts.Size), done: make(chan struct{})}
}

// Complete runs one completion in one of the Completer's sandboxes, with
// Runner.Complete's contract: the same request, answer and kinds of failure.
// The request's Timeout is one deadline over waiting for a free sandbox,
// creating one when the call needs to, and the turn. The request's
// SandboxTemplate, SandboxCPUs and SandboxMemory are ignored: the
// Completer's options decide them.
func (c *Completer) Complete(ctx context.Context, req CompletionRequest) (*Completion, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	name, timeout := req.callName(), req.timeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("%s session: %w", name, ErrCompleterClosed)
	}
	c.calls.Add(1)
	c.mu.Unlock()
	defer c.calls.Done()

	select {
	case c.slots <- struct{}{}:
	case <-c.done:
		return nil, fmt.Errorf("%s session: %w", name, ErrCompleterClosed)
	case <-ctx.Done():
		return nil, fmt.Errorf("%s session: waiting for a free sandbox: %w", name, ctx.Err())
	}
	defer func() { <-c.slots }()

	p, err := c.take(ctx)
	if err != nil {
		return nil, completionError(ctx, name, err)
	}
	turn, res, err := c.r.runCompletion(ctx, req, completionPlace{sandbox: p.box.name, workParent: p.workspace})
	// A turn that reported its end ran in a sandbox that works; anything
	// else may have left the sandbox broken, or claude running in it.
	c.give(p, err == nil && ctx.Err() == nil && res != nil && !res.TimedOut && turn.final != nil)
	if err != nil {
		return nil, completionError(ctx, name, err)
	}
	return classifyCompletion(name, timeout, turn, res)
}

// take returns a free sandbox, creating one when none is.
func (c *Completer) take(ctx context.Context) (*pooledSandbox, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrCompleterClosed
	}
	if n := len(c.idle); n > 0 {
		p := c.idle[n-1]
		c.idle = c.idle[:n-1]
		c.mu.Unlock()
		return p, nil
	}
	c.mu.Unlock()
	p, err := c.create(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		c.discard(p)
		return nil, ErrCompleterClosed
	}
	return p, nil
}

// give returns a sandbox after a call: kept for the next one when it is
// healthy and the Completer is open, removed otherwise.
func (c *Completer) give(p *pooledSandbox, healthy bool) {
	c.mu.Lock()
	if healthy && !c.closed {
		c.idle = append(c.idle, p)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	c.discard(p)
}

// create makes a sandbox for the Completer: an empty workspace under the
// host's temporary directory, named the way a session's own primary
// workspace is so orphan cleanup knows it (procs.SandboxWorkspaceFile), and
// `sbx create` with it as the only workspace. The session directory records
// the sandbox's name and its workspace while they exist, so a process that
// dies without Close leaves them for procs.CleanSandboxes.
func (c *Completer) create(ctx context.Context) (_ *pooledSandbox, err error) {
	r := c.r
	sessionDir, err := r.NewSessionDir(c.opts.Name)
	if err != nil {
		return nil, fmt.Errorf("create a sandbox for %s: %w", c.opts.Name, err)
	}
	p := &pooledSandbox{sessionDir: sessionDir}
	defer func() {
		if err != nil {
			c.discard(p)
		}
	}()
	dir, err := os.MkdirTemp("", r.namePrefix()+"sbx-primary-")
	if err != nil {
		return nil, fmt.Errorf("create a sandbox for %s: make its workspace: %w", c.opts.Name, err)
	}
	p.workspace = dir
	if err := os.WriteFile(filepath.Join(sessionDir, procs.SandboxWorkspaceFile), []byte(dir+"\n"), 0o644); err != nil {
		return nil, fmt.Errorf("create a sandbox for %s: record its workspace: %w", c.opts.Name, err)
	}
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return nil, fmt.Errorf("create a sandbox for %s: make its workspace: %w", c.opts.Name, err)
	}
	p.workspace = dir
	req := Request{
		Name:      c.opts.Name,
		Workspace: vcs.Directory(dir),
		Profile: Profile{
			Name: c.opts.Name, Agent: AgentClaude, Sandbox: SandboxSbx,
			SandboxImage: c.opts.SandboxTemplate, SandboxCPUs: c.opts.SandboxCPUs, SandboxMemory: c.opts.SandboxMemory,
		},
	}
	turn := &Turn{Binds: []Bind{{Source: dir, Destination: dir, Access: ReadWrite}}}
	box := &sandbox{container: container{
		r: r, req: req, sessionDir: sessionDir, turn: turn,
		name: sandboxName(r.namePrefix()+c.opts.Name, randomHex(4)), image: c.opts.SandboxTemplate, listen: r.sandboxListen,
	}}
	if err := box.create(ctx); err != nil {
		return nil, fmt.Errorf("create a sandbox for %s: %w", c.opts.Name, err)
	}
	p.box = box
	if err := procs.WriteSandboxName(sessionDir, box.name); err != nil {
		return nil, fmt.Errorf("create a sandbox for %s: record its name: %w", c.opts.Name, err)
	}
	return p, nil
}

// discard removes a sandbox, its workspace and its session directory, as
// far as each of them exists.
func (c *Completer) discard(p *pooledSandbox) {
	if p.box != nil {
		p.box.remove()
	}
	if p.workspace != "" {
		if err := os.RemoveAll(p.workspace); err != nil {
			c.r.Logger.Warn("remove completion sandbox workspace", "dir", p.workspace, "err", err)
		}
	}
	if err := os.RemoveAll(p.sessionDir); err != nil {
		c.r.Logger.Warn("remove completion sandbox session", "dir", p.sessionDir, "err", err)
	}
}

// Close removes every sandbox the Completer created: the free ones at once,
// and each one a call is still using when that call ends, which Close waits
// for until ctx ends. A call that Close finds waiting for a sandbox returns
// ErrCompleterClosed, and so does every call after it. Close is safe to call
// more than once and from several goroutines; it returns ctx's error when ctx
// ended before every call did, and nil otherwise.
func (c *Completer) Close(ctx context.Context) error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.done)
	}
	idle := c.idle
	c.idle = nil
	c.mu.Unlock()
	for _, p := range idle {
		c.discard(p)
	}
	finished := make(chan struct{})
	go func() {
		c.calls.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("close the completer: calls still running: %w", ctx.Err())
	}
}
