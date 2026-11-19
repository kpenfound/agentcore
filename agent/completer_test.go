package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// poolClaude is a fake claude that appends where it ran and how many files it
// found there, leaves a file behind, and sleeps first when the test left a
// marker asking for a slow turn.
const poolClaude = `
if [ -f "$COMPLETION_REC/slow" ]; then rm -f "$COMPLETION_REC/slow"; sleep 5; fi
if [ -f "$COMPLETION_REC/linger" ]; then sleep 0.3; fi
pwd >> "$COMPLETION_REC/pool-pwd.txt"
ls -A | wc -l | tr -d ' ' >> "$COMPLETION_REC/pool-files.txt"
echo private > left-behind.txt
cat > /dev/null
`

// creates is how many sandboxes `sbx create` was asked for.
func (f *completionFixture) creates(t *testing.T) int {
	t.Helper()
	n := 0
	for _, call := range lines(t, filepath.Join(f.sbx, "sbx-calls.txt")) {
		if strings.HasPrefix(call, "create ") {
			n++
		}
	}
	return n
}

func (f *completionFixture) linesOf(name string) []string {
	b, err := os.ReadFile(filepath.Join(f.rec, name))
	if err != nil {
		return nil
	}
	return strings.Fields(string(b))
}

// A Completer creates its sandbox once and runs each call in it, so a stream
// of calls is one kit resolution, not one per call. Each call starts in an
// empty directory of its own that is gone when it ends, so nothing one call
// was given or wrote is there for the next. Close removes the sandbox and
// everything the Completer made, and the Completer refuses calls after it.
func TestACompleterRunsSequentialCallsInOneSandbox(t *testing.T) {
	f := newCompletionFixture(t, poolClaude+okStream)
	before := f.entries(t)
	c := f.r.NewCompleter(CompleterOptions{Name: "distill"})

	for i := 0; i < 3; i++ {
		got, err := c.Complete(context.Background(), completionRequest())
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got.Text != "the answer" {
			t.Fatalf("call %d answered %q", i, got.Text)
		}
	}
	if n := f.creates(t); n != 1 {
		t.Errorf("three calls created %d sandboxes, want 1", n)
	}
	if f.exists("sbx-rm.txt") {
		t.Errorf("a sandbox was removed between calls: %v", f.sbxRecorded(t, "sbx-rm.txt"))
	}
	pwds := f.linesOf("pool-pwd.txt")
	if len(pwds) != 3 || pwds[0] == pwds[1] || pwds[1] == pwds[2] {
		t.Errorf("working directories %v, want one of its own per call", pwds)
	}
	for _, d := range pwds {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("call directory %s outlived its call", d)
		}
	}
	if files := f.linesOf("pool-files.txt"); !slices.Equal(files, []string{"0", "0", "0"}) {
		t.Errorf("files found at the start of each call: %v, want none", files)
	}

	name, _ := f.created(t)
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rm := f.sbxRecorded(t, "sbx-rm.txt"); !slices.Equal(rm, []string{"rm", "--force", name}) {
		t.Errorf("sbx rm: %v, want the sandbox removed", rm)
	}
	if after := f.entries(t); !slices.Equal(after, before) {
		t.Errorf("left behind: %v, want %v", after, before)
	}
	if _, err := c.Complete(context.Background(), completionRequest()); !errors.Is(err, ErrCompleterClosed) {
		t.Errorf("call after Close: %v, want ErrCompleterClosed", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// Size bounds the sandboxes alive at once: more calls than that at once wait
// for a free sandbox rather than creating another.
func TestACompleterCreatesNoMoreSandboxesThanItsSize(t *testing.T) {
	f := newCompletionFixture(t, poolClaude+okStream)
	if err := os.WriteFile(filepath.Join(f.rec, "linger"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	c := f.r.NewCompleter(CompleterOptions{Name: "distill", Size: 2})
	defer c.Close(context.Background())

	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Complete(context.Background(), completionRequest())
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := f.creates(t); n < 1 || n > 2 {
		t.Errorf("five concurrent calls created %d sandboxes, want at most 2", n)
	}
}

// A call that leaves the sandbox in doubt — sbx no longer has it, or claude
// may still be running in it after a timeout — costs that sandbox, and the
// next call gets a fresh one. A model's own failure, such as an answer with
// no structured output, does not: the sandbox worked.
func TestACompleterReplacesABrokenSandboxOnly(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, f *completionFixture, req *CompletionRequest)
		want    error
		creates int
	}{
		{"sandbox gone", func(t *testing.T, f *completionFixture, _ *CompletionRequest) { f.mark(t, "fail-exec") }, nil, 2},
		{"timed out", func(t *testing.T, f *completionFixture, req *CompletionRequest) {
			if err := os.WriteFile(filepath.Join(f.rec, "slow"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			req.Timeout = time.Second
		}, context.DeadlineExceeded, 2},
		{"no structured output", func(_ *testing.T, _ *completionFixture, req *CompletionRequest) {
			req.Schema = []byte(`{"type":"object"}`)
		}, ErrNoStructuredOutput, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCompletionFixture(t, poolClaude+okStream)
			c := f.r.NewCompleter(CompleterOptions{Name: "distill"})
			defer c.Close(context.Background())
			if _, err := c.Complete(context.Background(), completionRequest()); err != nil {
				t.Fatal(err)
			}
			first, _ := f.created(t)

			req := completionRequest()
			tc.prepare(t, f, &req)
			_, err := c.Complete(context.Background(), req)
			if err == nil {
				t.Fatal("the failing call succeeded")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("failure %v, want %v", err, tc.want)
			}
			if _, err := c.Complete(context.Background(), completionRequest()); err != nil {
				t.Fatalf("the call after: %v", err)
			}
			if n := f.creates(t); n != tc.creates {
				t.Errorf("created %d sandboxes, want %d", n, tc.creates)
			}
			removed := f.exists("sbx-rm.txt") && slices.Contains(f.sbxRecorded(t, "sbx-rm.txt"), first)
			if removed != (tc.creates == 2) {
				t.Errorf("first sandbox removed: %v, want %v", removed, tc.creates == 2)
			}
		})
	}
}

// A registry that refuses sbx for its pull rate limit is told apart from
// other failures, by Runner.Complete and by a Completer alike, so a caller
// can wait for the window instead of retrying at once; neither leaves
// anything behind.
func TestASandboxCreateRefusedForTheRateLimitIsErrRegistryRateLimited(t *testing.T) {
	for _, via := range []string{"Runner.Complete", "Completer"} {
		t.Run(via, func(t *testing.T) {
			f := newCompletionFixture(t, poolClaude+okStream)
			f.mark(t, "rate-limit-create")
			before := f.entries(t)
			var err error
			if via == "Completer" {
				c := f.r.NewCompleter(CompleterOptions{Name: "distill"})
				_, err = c.Complete(context.Background(), completionRequest())
				if cerr := c.Close(context.Background()); cerr != nil {
					t.Fatal(cerr)
				}
			} else {
				_, err = f.r.Complete(context.Background(), completionRequest())
			}
			if !errors.Is(err, ErrRegistryRateLimited) {
				t.Fatalf("failure %v, want ErrRegistryRateLimited", err)
			}
			if after := f.entries(t); !slices.Equal(after, before) {
				t.Errorf("left behind: %v, want %v", after, before)
			}
		})
	}
}

// removes is how many times `sbx rm` ran.
func (f *completionFixture) removes(t *testing.T) int {
	t.Helper()
	n := 0
	for _, call := range lines(t, filepath.Join(f.sbx, "sbx-calls.txt")) {
		if strings.HasPrefix(call, "rm ") {
			n++
		}
	}
	return n
}

// eventually polls cond until it holds, failing the test after a generous
// deadline: the reaper runs on its own time.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A sandbox kept past its IdleTimeout is removed, so an idle Completer holds
// no VM, and the next call creates a fresh one.
func TestAnIdleSandboxIsRemovedAfterItsIdleTimeout(t *testing.T) {
	f := newCompletionFixture(t, poolClaude+okStream)
	before := f.entries(t)
	c := f.r.NewCompleter(CompleterOptions{Name: "distill", IdleTimeout: 50 * time.Millisecond})

	if _, err := c.Complete(context.Background(), completionRequest()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the idle sandbox to be removed", func() bool { return f.removes(t) == 1 })
	// sbx rm runs before the workspace and session directories go.
	eventually(t, "the removed sandbox's directories to go", func() bool { return slices.Equal(f.entries(t), before) })
	if _, err := c.Complete(context.Background(), completionRequest()); err != nil {
		t.Fatal(err)
	}
	if n := f.creates(t); n != 2 {
		t.Errorf("creates = %d, want a fresh sandbox for the call after the removal", n)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.removes(t); n != 2 {
		t.Errorf("removes after Close = %d, want the second sandbox removed too", n)
	}
	if after := f.entries(t); !slices.Equal(after, before) {
		t.Errorf("left behind after Close: %v, want %v", after, before)
	}
}

// Calls that keep arriving within the IdleTimeout share one sandbox: the
// timeout empties a pool between bursts, never during one.
func TestCallsWithinTheIdleTimeoutReuseTheSandbox(t *testing.T) {
	f := newCompletionFixture(t, poolClaude+okStream)
	c := f.r.NewCompleter(CompleterOptions{Name: "distill", IdleTimeout: 5 * time.Second})
	defer c.Close(context.Background())

	for i := 0; i < 3; i++ {
		if _, err := c.Complete(context.Background(), completionRequest()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if creates, removes := f.creates(t), f.removes(t); creates != 1 || removes != 0 {
		t.Errorf("creates, removes = %d, %d; want one sandbox kept through the burst", creates, removes)
	}
}

// A sandbox a call is using is never removed for being idle, however long the
// call runs; it becomes idle, and can be removed, only once the call ends.
func TestABusySandboxIsNotRemovedForBeingIdle(t *testing.T) {
	f := newCompletionFixture(t, poolClaude+okStream)
	c := f.r.NewCompleter(CompleterOptions{Name: "distill", IdleTimeout: 100 * time.Millisecond})
	defer c.Close(context.Background())
	if _, err := c.Complete(context.Background(), completionRequest()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the first sandbox to be removed", func() bool { return f.removes(t) == 1 })

	// Each turn now outlasts the IdleTimeout three times over. The check
	// right after it returns has the whole IdleTimeout before the sandbox,
	// idle again, may go.
	if err := os.WriteFile(filepath.Join(f.rec, "linger"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Complete(context.Background(), completionRequest()); err != nil {
		t.Fatal(err)
	}
	if n := f.removes(t); n != 1 {
		t.Errorf("removes when the long call returned = %d, want 1: its sandbox was removed while in use", n)
	}
	eventually(t, "the sandbox to be removed once idle", func() bool { return f.removes(t) == 2 })
}

// Close after the reaper has removed a sandbox is clean, and stops it: nothing
// is removed after Close returns.
func TestCloseStopsTheReaper(t *testing.T) {
	f := newCompletionFixture(t, poolClaude+okStream)
	before := f.entries(t)
	c := f.r.NewCompleter(CompleterOptions{Name: "distill", Size: 2, IdleTimeout: 30 * time.Millisecond})
	if _, err := c.Complete(context.Background(), completionRequest()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the idle sandbox to be removed", func() bool { return f.removes(t) == 1 })
	if _, err := c.Complete(context.Background(), completionRequest()); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	removed := f.removes(t)
	time.Sleep(150 * time.Millisecond) // several IdleTimeouts: a reaper still running would act
	if n := f.removes(t); n != removed || n != 2 {
		t.Errorf("removes = %d after Close and %d later, want 2 and no more", removed, n)
	}
	if after := f.entries(t); !slices.Equal(after, before) {
		t.Errorf("left behind: %v, want %v", after, before)
	}
}
