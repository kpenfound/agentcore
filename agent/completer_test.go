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
