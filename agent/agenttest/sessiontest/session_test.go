// Package sessiontest checks the public fixture in a binary with no TestMain.
package sessiontest_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest"
	"github.com/kpenfound/agentcore/vcs"
	"github.com/kpenfound/agentcore/work"
)

func request(t *testing.T, backend string) (agent.Runner, agent.Request) {
	t.Helper()
	dir := t.TempDir()
	session := filepath.Join(dir, "session")
	if err := os.Mkdir(session, 0o755); err != nil {
		t.Fatal(err)
	}
	r := agent.Runner{SessionsDir: dir}
	req := agent.Request{Name: "scripted", Workspace: vcs.Directory(dir), SessionDir: session, Profile: agent.Profile{Agent: backend, Timeout: 5 * time.Second, VCSAccess: true}, Prompt: "Review 'this'\nwith a second line", Grants: &agent.Grants{VCS: true, Tools: []string{agent.ToolsAll}, Mounts: []agent.Mount{{Path: "/", Access: agent.ReadWrite}}}}
	return r, req
}
func use(r *agent.Runner, backend string, f *agenttest.SessionFixture) {
	if backend == agent.AgentClaude {
		r.ClaudeBin = f.Bin
	} else {
		r.CodexBin = f.Bin
	}
}
func run(t *testing.T, r agent.Runner, req agent.Request, restricted bool) (*agent.Result, error) {
	t.Helper()
	if restricted {
		req.Profile.VCSAccess = false
		req.Grants = nil
		req.SessionDir = ""
		rr, err := r.RunRestricted(context.Background(), req)
		if err != nil {
			return nil, err
		}
		return rr.Result, nil
	}
	return r.Run(context.Background(), req)
}
func checkResult(t *testing.T, res *agent.Result, backend string, turn agenttest.Turn) {
	t.Helper()
	if res == nil || res.IsError || res.ResultText != turn.ResultText || res.ClaudeID != turn.SessionID || res.NumTurns != turn.NumTurns {
		t.Fatalf("result: %+v", res)
	}
	if backend == agent.AgentClaude {
		if !res.CostKnown || res.CostUSD != turn.CostUSD {
			t.Fatalf("claude cost: %+v", res)
		}
	} else if res.CostKnown || res.CostUSD != 0 {
		t.Fatalf("codex cost: %+v", res)
	}
}
func TestSessionPaths(t *testing.T) {
	for _, backend := range []string{agent.AgentClaude, agent.AgentCodex} {
		for _, mode := range []string{"ordinary", "held", "restricted", "container", "sbx"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				turn := agenttest.Turn{ResultText: "answer 'quoted'\n$HOME `literal` \\\" 世界", SessionID: "session-thread", NumTurns: 3, CostUSD: 0.25}
				f := agenttest.Session(t, backend, turn)
				r, req := request(t, backend)
				use(&r, backend, f)
				if mode == "held" {
					if backend == agent.AgentClaude {
						req.Grants.Tools = []string{"Read"}
					} else {
						req.Grants.Tools = []string{"update_plan"}
					}
				}
				if mode == "container" || mode == "sbx" {
					// The engine scripts record in their working directory using
					// the shell's PWD, with no extra environment grant.
					req.Profile.VCSAccess = false
					req.Grants.VCS = false
					req.Grants.Mounts = []agent.Mount{{Path: req.Workspace.Directory(), Access: agent.ReadWrite}}
					req.Profile.SandboxImage = "image"
					if mode == "container" {
						req.Profile.Sandbox = agent.SandboxContainer
						r.DockerBin = agenttest.Docker(t, "image", "PWD")
						r.ContainerListen = "127.0.0.1:0"
					} else {
						req.Profile.Sandbox = agent.SandboxSbx
						r.SbxBin = agenttest.Sbx(t, "PWD")
					}
				}
				res, err := run(t, r, req, mode == "restricted")
				if err != nil {
					t.Fatal(err)
				}
				checkResult(t, res, backend, turn)
				rec := f.Record(t)
				if rec.Starts != 1 {
					t.Fatalf("record: %+v", rec)
				}
				inv := rec.Sessions[0]
				if strings.TrimSuffix(inv.Prompt, "\n") != req.Prompt || inv.WorkingDirectory != req.Workspace.Directory() || len(inv.Args) == 0 {
					t.Fatalf("invocation: %+v", inv)
				}
				if mode == "held" && backend == agent.AgentCodex && !slices.Contains(inv.Methods, "config/read") {
					t.Fatalf("held conversation: %+v", inv)
				}
			})
		}
	}
}
func TestSessionOutcomeAndFiles(t *testing.T) {
	for _, backend := range []string{agent.AgentClaude, agent.AgentCodex} {
		t.Run(backend, func(t *testing.T) {
			outcome := agenttest.SessionOutcome{Status: "ready", Note: "checked", Work: work.Ref{Key: "opaque", Tags: map[string]string{"kind": "test"}}}
			f := agenttest.Session(t, backend, agenttest.Turn{Outcome: &outcome, Files: map[string]string{"nested/a 'file'": "bytes\n$() `literal`\x00"}, DeleteFiles: []string{"old"}})
			r, req := request(t, backend)
			use(&r, backend, f)
			if err := os.WriteFile(filepath.Join(req.Workspace.Directory(), "old"), []byte("old"), 0o644); err != nil {
				t.Fatal(err)
			}
			res, err := r.Run(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if !res.HasOutcome || res.Outcome.Status != outcome.Status || res.Outcome.Note != outcome.Note || !reflect.DeepEqual(res.Outcome.Work, outcome.Work) {
				t.Fatalf("outcome: %+v", res)
			}
			b, err := os.ReadFile(filepath.Join(req.Workspace.Directory(), "nested/a 'file'"))
			if err != nil || string(b) != "bytes\n$() `literal`\x00" {
				t.Fatalf("file: %q, %v", b, err)
			}
			if _, err := os.Stat(filepath.Join(req.Workspace.Directory(), "old")); !os.IsNotExist(err) {
				t.Fatalf("delete: %v", err)
			}
		})
	}
}
func TestSessionFailures(t *testing.T) {
	reset := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		name, backend, resume string
		turn                  agenttest.Turn
	}{
		{name: "claude error", backend: agent.AgentClaude, turn: agenttest.Turn{End: agenttest.TurnError, ResultText: "scripted error", ErrorSubtype: "error_max_turns", ExitCode: 7}},
		{name: "claude limited", backend: agent.AgentClaude, turn: agenttest.Turn{End: agenttest.TurnRateLimited, ResetAt: reset}},
		{name: "codex failed", backend: agent.AgentCodex, turn: agenttest.Turn{End: agenttest.TurnFailed, ResultText: "scripted failure"}},
		{name: "codex thread start", backend: agent.AgentCodex, turn: agenttest.Turn{End: agenttest.TurnThreadError, ResultText: "cannot open thread"}},
		{name: "codex thread resume", backend: agent.AgentCodex, resume: "old-thread", turn: agenttest.Turn{End: agenttest.TurnThreadError, ResultText: "cannot resume thread"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := agenttest.Session(t, tc.backend, tc.turn)
			r, req := request(t, tc.backend)
			use(&r, tc.backend, f)
			req.ResumeID = tc.resume
			res, err := r.Run(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if tc.turn.End == agenttest.TurnRateLimited {
				got, limited := res.SessionLimited()
				if !limited || !got.Equal(reset) {
					t.Fatalf("limit: %+v", res)
				}
			} else if !res.IsError || !strings.Contains(res.ResultText, tc.turn.ResultText) {
				t.Fatalf("failure: %+v", res)
			}
			if tc.turn.End == agenttest.TurnError && (res.ErrorSubtype != tc.turn.ErrorSubtype || res.ExitCode != 7) {
				t.Fatalf("claude error: %+v", res)
			}
			rec := f.Record(t)
			if rec.Starts != 1 || rec.Sessions[0].ResumeID != tc.resume {
				t.Fatalf("record: %+v", rec)
			}
		})
	}
}
func TestSessionTimeoutAndCancellation(t *testing.T) {
	for _, backend := range []string{agent.AgentClaude, agent.AgentCodex} {
		t.Run(backend, func(t *testing.T) {
			f := agenttest.Session(t, backend, agenttest.Turn{End: agenttest.TurnNeverEnds})
			r, req := request(t, backend)
			use(&r, backend, f)
			req.Profile.Timeout = 200 * time.Millisecond
			res, err := r.Run(context.Background(), req)
			if err != nil || res == nil || !res.TimedOut {
				t.Fatalf("timeout: %+v, %v", res, err)
			}
			if f.Record(t).Starts != 1 {
				t.Fatal("no session started")
			}
		})
	}
	t.Run("cancel claude", func(t *testing.T) {
		f := agenttest.Session(t, agent.AgentClaude, agenttest.Turn{End: agenttest.TurnNeverEnds})
		r, req := request(t, agent.AgentClaude)
		use(&r, agent.AgentClaude, f)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ended := make(chan error, 1)
		go func() { _, err := r.Run(ctx, req); ended <- err }()
		deadline := time.Now().Add(3 * time.Second)
		for f.Record(t).Starts == 0 {
			if time.Now().After(deadline) {
				t.Fatal("session never started")
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		if err := <-ended; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
		if _, err := os.Stat(filepath.Join(req.SessionDir, agent.ResultFile)); !os.IsNotExist(err) {
			t.Fatalf("cancel wrote result: %v", err)
		}
	})
}

func TestSessionSequenceAndRecords(t *testing.T) {
	for _, backend := range []string{agent.AgentClaude, agent.AgentCodex} {
		for _, mode := range []string{"ordinary", "held", "restricted"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				failure := agenttest.Turn{ResultText: "second fails", SessionID: "second", End: agenttest.TurnError, ErrorSubtype: "scripted_failure", ExitCode: 2}
				if backend == agent.AgentCodex {
					failure.End = agenttest.TurnFailed
					failure.ExitCode = 0
				}
				f := agenttest.Session(t, backend, agenttest.Turn{ResultText: "first", SessionID: "first"}, failure)
				for i := 0; i < 3; i++ {
					r, req := request(t, backend)
					use(&r, backend, f)
					req.Prompt = "prompt\nwith \\\" quotes and trailing newline\n"
					req.Env = map[string]string{"CALLER_VALUE": "value\nwith newline"}
					req.Grants.Env = []string{"CALLER_VALUE"} // host fake needs no PATH
					req.ResumeID = "previous-id"
					if mode == "held" {
						if backend == agent.AgentClaude {
							req.Grants.Tools = []string{"Read"}
						} else {
							req.Grants.Tools = []string{"update_plan"}
						}
					}
					res, err := run(t, r, req, mode == "restricted")
					if i == 0 {
						if err != nil || res.IsError || res.ResultText != "first" || res.ClaudeID != "first" {
							t.Fatalf("first: %+v %v", res, err)
						}
					} else if mode == "restricted" {
						if err == nil || !strings.Contains(err.Error(), failure.ResultText) {
							t.Fatalf("restricted failure: %+v %v", res, err)
						}
					} else if err != nil || !res.IsError || res.ResultText != failure.ResultText {
						t.Fatalf("failure %d: %+v %v", i, res, err)
					}
					rec := f.Record(t)
					if rec.Starts != i+1 {
						t.Fatalf("starts: %+v", rec)
					}
					inv := rec.Sessions[i]
					if inv.Prompt != req.Prompt || inv.ResumeID != req.ResumeID || inv.WorkingDirectory != req.Workspace.Directory() || inv.Environment["CALLER_VALUE"] != req.Env["CALLER_VALUE"] {
						t.Fatalf("recorded start: %+v", inv)
					}
					if mode != "restricted" {
						if _, ok := inv.Environment["PATH"]; ok {
							t.Fatal("fixture required PATH")
						}
					}
				}
				rec := f.Record(t)
				wantProbes := 0
				if mode == "restricted" && backend == agent.AgentCodex {
					wantProbes = 3
				}
				if len(rec.Probes) != wantProbes {
					t.Fatalf("probes: %+v", rec)
				}
				for _, p := range rec.Probes {
					if len(p.Args) < 2 || p.Args[0] != "mcp" || p.Args[1] != "list" {
						t.Fatalf("probe: %+v", p)
					}
				}
			})
		}
	}
}

func TestSessionOnPATHAndSeparateProcess(t *testing.T) {
	for _, backend := range []string{agent.AgentClaude, agent.AgentCodex} {
		t.Run(backend, func(t *testing.T) {
			turn := agenttest.Turn{ResultText: "standalone answer", SessionID: "standalone", NumTurns: 1, CostUSD: 0.1}
			f := agenttest.Session(t, backend, turn)
			t.Setenv("PATH", f.Dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			r, req := request(t, backend) // executable override intentionally empty
			res, err := r.Run(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			checkResult(t, res, backend, turn)
			transcript, err := os.ReadFile(res.Transcript)
			if err != nil {
				t.Fatal(err)
			}
			// A separately compiled caller uses PATH and the exported Runner API.
			// Its transcript must match, including both directions for Codex.
			output := standaloneCaller(t, backend, req)
			if string(output) != string(transcript) {
				t.Fatalf("standalone stream differs:\n%s\nwant:\n%s", output, transcript)
			}
			if f.Record(t).Starts != 2 {
				t.Fatal("separate caller did not start a session")
			}
		})
	}
}

func standaloneCaller(t *testing.T, backend string, req agent.Request) []byte {
	t.Helper()
	dir := t.TempDir()
	source := `package main
import (
 "context"
 "fmt"
 "os"
 "time"
 "github.com/kpenfound/agentcore/agent"
 "github.com/kpenfound/agentcore/vcs"
)
func main() {
 r:=agent.Runner{SessionsDir:os.Args[2]}
 req:=agent.Request{Name:"scripted",Profile:agent.Profile{Agent:os.Args[1],VCSAccess:true,Timeout:5*time.Second},Workspace:vcs.Directory(os.Args[2]),Prompt:os.Args[3],Grants:&agent.Grants{VCS:true,Tools:[]string{agent.ToolsAll},Mounts:[]agent.Mount{{Path:"/",Access:agent.ReadWrite}}}}
 result,err:=r.Run(context.Background(),req)
 if err!=nil {panic(err)}
 known:=os.Args[1]==agent.AgentClaude
 if result.IsError || result.ResultText!="standalone answer" || result.ClaudeID!="standalone" || result.CostKnown!=known {panic(fmt.Sprintf("standalone runner: %+v",result))}
 transcript,err:=os.ReadFile(result.Transcript)
 if err!=nil {panic(err)}
 if _,err=os.Stdout.Write(transcript);err!=nil {panic(err)}
}
`
	filename := filepath.Join(dir, "main.go")
	if err := os.WriteFile(filename, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "caller")
	cmd := exec.Command("go", "build", "-o", bin, filename)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build caller: %s %v", output, err)
	}
	cmd = exec.Command(bin, backend, req.Workspace.Directory(), req.Prompt)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("caller: %s %v", output, err)
	}
	return output
}

func TestConcurrentSessionClaims(t *testing.T) {
	f := agenttest.Session(t, agent.AgentClaude, agenttest.Turn{ResultText: "one"}, agenttest.Turn{ResultText: "two"}, agenttest.Turn{ResultText: "three"})
	var wg sync.WaitGroup
	outputs := make(chan string, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := exec.Command(f.Bin, "-p").CombinedOutput()
			if err != nil {
				t.Errorf("concurrent fake: %v %s", err, out)
			}
			outputs <- string(out)
		}()
	}
	wg.Wait()
	close(outputs)
	var got []string
	for out := range outputs {
		for _, want := range []string{"one", "two", "three"} {
			if strings.Contains(out, want) {
				got = append(got, want)
			}
		}
	}
	slices.Sort(got)
	if !reflect.DeepEqual(got, []string{"one", "three", "two"}) || f.Record(t).Starts != 3 {
		t.Fatalf("claims: %v %+v", got, f.Record(t))
	}
}

func TestUnknownInvocationIsVisibleProbe(t *testing.T) {
	for _, backend := range []string{agent.AgentClaude, agent.AgentCodex} {
		t.Run(backend, func(t *testing.T) {
			f := agenttest.Session(t, backend, agenttest.Turn{})
			output, err := exec.Command(f.Bin, "unknown-command").CombinedOutput()
			if err == nil || !strings.Contains(string(output), "unrecognized") {
				t.Fatalf("unknown command: %s %v", output, err)
			}
			rec := f.Record(t)
			if rec.Starts != 0 || len(rec.Probes) != 1 {
				t.Fatalf("unknown probe: %+v", rec)
			}
		})
	}
}
