package agenttest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/work"
)

// TurnEnd describes how a scripted session ends. The zero value succeeds.
type TurnEnd string

const (
	TurnSuccess     TurnEnd = ""
	TurnError       TurnEnd = "error"        // Claude error result; ErrorSubtype and ExitCode apply.
	TurnRateLimited TurnEnd = "rate-limited" // Claude blocking rate-limit event with ResetAt.
	TurnFailed      TurnEnd = "failed"       // Codex error notification and failed completion; ResultText is the error.
	TurnThreadError TurnEnd = "thread-error" // Codex error answering thread start or resume.
	TurnNeverEnds   TurnEnd = "never-ends"   // Wait for the runner to stop the process.
)

// SessionOutcome is written directly to the session's outcome file. The fake
// does not call MCP tools or make decisions about the caller's workflow.
type SessionOutcome struct {
	Work   work.Ref `json:"work"`
	Status string   `json:"status"`
	Note   string   `json:"note,omitempty"`
}

// Turn describes one session. NumTurns defaults to one. SessionID is Claude's
// session id or Codex's thread id. CostUSD is reported only by Claude. Files
// writes workspace-relative paths (creating parents); DeleteFiles removes them.
// Changes and Outcome are written before the scripted ending, including a wait.
type Turn struct {
	ResultText   string
	SessionID    string
	NumTurns     int
	CostUSD      float64
	Outcome      *SessionOutcome
	Files        map[string]string
	DeleteFiles  []string
	End          TurnEnd
	ErrorSubtype string
	ExitCode     int
	ResetAt      time.Time
}

// SessionFixture is a standalone fake CLI, independent of the test binary and
// its environment. Dir contains Bin under the backend's default name and may
// be prepended to PATH. State and records live there too; real kernel
// confinement of those files is outside this fixture's contract.
type SessionFixture struct {
	Bin string
	Dir string
}

// Session writes a standalone claude or codex executable. Each session start
// claims the next turn, repeating the last. Probes get automatic answers and
// consume no turn. No TestMain, environment variable or extra grant is needed.
// The executable uses /bin/sh and absolute paths to standard system utilities,
// so a grant may omit PATH. Unknown invocations fail visibly and are recorded.
func Session(t *testing.T, backend string, turns ...Turn) *SessionFixture {
	t.Helper()
	if backend != "claude" && backend != "codex" {
		t.Fatalf("agenttest.Session: unsupported backend %q", backend)
	}
	if len(turns) == 0 {
		t.Fatal("agenttest.Session: at least one turn is required")
	}
	f := &SessionFixture{Dir: t.TempDir()}
	f.Bin = filepath.Join(f.Dir, backend)
	var script strings.Builder
	script.WriteString("#!/bin/sh\n# Standalone agenttest session fixture.\nroot=" + shellQuote(f.Dir) + "\nbackend=" + shellQuote(backend) + "\n")
	script.WriteString(sessionPrelude)
	script.WriteString("load_turn() {\ncase $turn in\n")
	for i, turn := range turns {
		if turn.NumTurns == 0 {
			turn.NumTurns = 1
		}
		if turn.NumTurns < 0 || turn.ExitCode < 0 || turn.ExitCode > 255 {
			t.Fatal("agenttest.Session: invalid turn count or exit code")
		}
		switch turn.End {
		case TurnSuccess, TurnNeverEnds:
		case TurnError, TurnRateLimited:
			if backend != "claude" {
				t.Fatalf("agenttest.Session: %q requires claude", turn.End)
			}
		case TurnFailed, TurnThreadError:
			if backend != "codex" {
				t.Fatalf("agenttest.Session: %q requires codex", turn.End)
			}
		default:
			t.Fatalf("agenttest.Session: unknown ending %q", turn.End)
		}
		pattern := strconv.Itoa(i + 1)
		if i == len(turns)-1 {
			pattern = "*"
		}
		fmt.Fprintf(&script, "%s)\nending=%s\nexit_code=%d\n", pattern, shellQuote(string(turn.End)), turn.ExitCode)
		fmt.Fprintf(&script, "thread_json=%s\nmessage_json=%s\n", shellQuote(sessionJSON(turn.SessionID)), shellQuote(sessionJSON(turn.ResultText)))
		subtype := "success"
		if turn.End == TurnError {
			subtype = turn.ErrorSubtype
			if subtype == "" {
				subtype = "error"
			}
		}
		result := map[string]any{"type": "result", "subtype": subtype, "is_error": turn.End == TurnError, "result": turn.ResultText, "session_id": turn.SessionID, "num_turns": turn.NumTurns, "total_cost_usd": turn.CostUSD}
		fmt.Fprintf(&script, "claude_result=%s\n", shellQuote(sessionJSON(result)))
		fmt.Fprintf(&script, "rate_event=%s\n", shellQuote(sessionJSON(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"status": "rejected", "rateLimitType": "five_hour", "resetsAt": turn.ResetAt.Unix()}})))
		var actions strings.Builder
		for _, name := range sortedSessionFiles(turn.Files) {
			validateSessionPath(t, name)
			fmt.Fprintf(&actions, "/bin/mkdir -p -- %s || exit 1\nprintf '%%b' %s > %s || exit 1\n", shellQuote(filepath.Dir(name)), shellQuote(shellBytes(turn.Files[name])), shellQuote(name))
		}
		for _, name := range turn.DeleteFiles {
			validateSessionPath(t, name)
			fmt.Fprintf(&actions, "/bin/rm -f -- %s || exit 1\n", shellQuote(name))
		}
		if turn.Outcome != nil {
			fmt.Fprintf(&actions, "[ -n \"$session_dir\" ] || { echo 'agenttest: no session directory for outcome' >&2; exit 1; }\nprintf '%%s' %s > \"$session_dir/outcome.json\" || exit 1\n", shellQuote(sessionJSON(turn.Outcome)))
		}
		fmt.Fprintf(&script, "actions=%s\n", shellQuote(actions.String()))
		var events strings.Builder
		for n := 0; n < turn.NumTurns; n++ {
			events.WriteString(sessionJSON(map[string]any{"jsonrpc": "2.0", "method": "item/completed", "params": map[string]any{"item": map[string]any{"type": "agentMessage", "phase": "final_answer", "text": turn.ResultText}}}) + "\n")
		}
		status := "completed"
		completed := map[string]any{}
		if turn.End == TurnFailed {
			status = "failed"
			completed["error"] = map[string]any{"message": turn.ResultText}
			events.WriteString(sessionJSON(map[string]any{"jsonrpc": "2.0", "method": "error", "params": map[string]any{"message": turn.ResultText}}) + "\n")
		}
		completed["status"] = status
		events.WriteString(sessionJSON(map[string]any{"jsonrpc": "2.0", "method": "turn/completed", "params": map[string]any{"turn": completed}}) + "\n")
		fmt.Fprintf(&script, "events=%s\n;;\n", shellQuote(events.String()))
	}
	script.WriteString("esac\n}\n")
	script.WriteString(sessionDriver)
	if err := os.WriteFile(f.Bin, []byte(script.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func sessionJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func sortedSessionFiles(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func validateSessionPath(t *testing.T, p string) {
	t.Helper()
	if !filepath.IsLocal(p) {
		t.Fatalf("agenttest.Session: file path must be workspace-relative: %q", p)
	}
}

// SessionInvocation records an executable invocation. Methods lists Codex's
// received request/notification methods, without exposing wire-format parsing to
// callers. A failed thread start has no Prompt: the runner has not sent it yet.
type SessionInvocation struct {
	Args             []string
	Environment      map[string]string
	WorkingDirectory string
	Prompt           string
	ResumeID         string
	Methods          []string
}

// SessionRecord separates sessions from probes. Starts equals len(Sessions).
// Sessions are in turn-claim order; probes are in process-start order.
type SessionRecord struct {
	Starts   int
	Sessions []SessionInvocation
	Probes   []SessionInvocation
}

// Record reads the fixture's record, including invocations still running. A
// failed record write under kernel confinement does not stop a scripted turn.
func (f *SessionFixture) Record(t *testing.T) SessionRecord {
	t.Helper()
	entries, err := os.ReadDir(f.Dir)
	if err != nil {
		t.Fatal(err)
	}
	type ordered struct {
		n          int
		invocation SessionInvocation
	}
	var sessions, probes []ordered
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "invocation-") {
			continue
		}
		dir := filepath.Join(f.Dir, entry.Name())
		read := func(name string) string {
			b, e := os.ReadFile(filepath.Join(dir, name))
			if e != nil && !os.IsNotExist(e) {
				t.Fatal(e)
			}
			return string(b)
		}
		nul := func(s string) []string {
			if s == "" {
				return nil
			}
			return strings.Split(strings.TrimSuffix(s, "\x00"), "\x00")
		}
		inv := SessionInvocation{Args: nul(read("args")), Environment: map[string]string{}, WorkingDirectory: read("cwd"), Prompt: read("prompt")}
		for _, kv := range nul(read("env")) {
			k, v, ok := strings.Cut(kv, "=")
			if ok {
				inv.Environment[k] = v
			}
		}
		for i, arg := range inv.Args {
			if arg == "--resume" && i+1 < len(inv.Args) {
				inv.ResumeID = inv.Args[i+1]
			}
		}
		// A live invocation may still be appending its final request. Decode
		// only complete lines; the next read will include the remainder.
		requests := read("requests")
		requests = requests[:strings.LastIndex(requests, "\n")+1]
		for _, line := range strings.Split(requests, "\n") {
			if line == "" {
				continue
			}
			var req struct {
				Method string `json:"method"`
				Params struct {
					ThreadID string `json:"threadId"`
					Input    []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"input"`
				} `json:"params"`
			}
			if err := json.Unmarshal([]byte(line), &req); err != nil {
				t.Fatalf("agenttest record request: %v", err)
			}
			if req.Method != "" {
				inv.Methods = append(inv.Methods, req.Method)
			}
			if req.Method == "thread/resume" {
				inv.ResumeID = req.Params.ThreadID
			}
			if req.Method == "turn/start" {
				for _, item := range req.Params.Input {
					if item.Type == "text" {
						inv.Prompt += item.Text
					}
				}
			}
		}
		if ordinal := strings.TrimSpace(read("session")); ordinal != "" {
			n, e := strconv.Atoi(ordinal)
			if e != nil {
				t.Fatal(e)
			}
			sessions = append(sessions, ordered{n, inv})
		} else {
			n, _ := strconv.Atoi(strings.TrimPrefix(entry.Name(), "invocation-"))
			probes = append(probes, ordered{n, inv})
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].n < sessions[j].n })
	sort.Slice(probes, func(i, j int) bool { return probes[i].n < probes[j].n })
	record := SessionRecord{Starts: len(sessions)}
	for _, s := range sessions {
		record.Sessions = append(record.Sessions, s.invocation)
	}
	for _, p := range probes {
		record.Probes = append(record.Probes, p.invocation)
	}
	return record
}

func shellBytes(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&b, "\\%03o", s[i])
	}
	return b.String()
}
