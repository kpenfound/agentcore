package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent/agenttest"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const callerToken = "caller-secret-token"

// callerFake is a caller-served MCP server over streamable HTTP that records
// what reached it: how many initializations, tool listings and session
// deletions, and how many requests carried the wrong token. A stall holds the
// requests of one JSON-RPC method until the client gives up on them.
type callerFake struct {
	URL string
	srv *mcp.Server

	mu                                               sync.Mutex
	requests, initializes, listings, deletes, unauth int
	stall                                            string
	stalled                                          chan struct{} // closed once a stalled request's context ended
	stalledOnce                                      sync.Once
	release                                          chan struct{}
}

func callerTool(name string, a *mcp.ToolAnnotations) (*mcp.Tool, mcp.ToolHandler) {
	return &mcp.Tool{Name: name, Description: name, InputSchema: map[string]any{"type": "object"}, Annotations: a},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return textResult("answer from " + name), nil
		}
}

func readOnlyAnnotation() *mcp.ToolAnnotations { return &mcp.ToolAnnotations{ReadOnlyHint: true} }

func newCallerFake(t *testing.T, tools ...*mcp.Tool) *callerFake {
	t.Helper()
	f := &callerFake{stalled: make(chan struct{}), release: make(chan struct{})}
	f.srv = mcp.NewServer(&mcp.Implementation{Name: "caller", Version: "1"}, nil)
	for _, tool := range tools {
		_, h := callerTool(tool.Name, nil)
		f.srv.AddTool(tool, h)
	}
	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return f.srv }, nil)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.serve(inner, w, r)
	}))
	f.URL = hs.URL + "/mcp"
	t.Cleanup(hs.Close)
	t.Cleanup(func() { close(f.release) })
	return f
}

func (f *callerFake) serve(inner http.Handler, w http.ResponseWriter, r *http.Request) {
	var method string
	if r.Method == http.MethodPost {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var msg struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &msg)
		method = msg.Method
	}
	f.mu.Lock()
	f.requests++
	switch {
	case r.Header.Get("Authorization") != "Bearer "+callerToken:
		f.unauth++
		f.mu.Unlock()
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	case r.Method == http.MethodDelete:
		f.deletes++
	case method == "initialize":
		f.initializes++
	case method == "tools/list":
		f.listings++
	}
	stall := f.stall != "" && f.stall == method
	f.mu.Unlock()
	if stall {
		select {
		case <-r.Context().Done():
			f.stalledOnce.Do(func() { close(f.stalled) })
		case <-f.release:
		}
		return
	}
	inner.ServeHTTP(w, r)
}

func (f *callerFake) counts() (requests, initializes, listings, deletes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests, f.initializes, f.listings, f.deletes
}

func (f *callerFake) server() *CallerMCPServer {
	return &CallerMCPServer{Name: "kern_codemode", URL: f.URL, BearerToken: callerToken}
}

func callerRequest(agentName string, f *callerFake) Request {
	req := restrictedRequestFor(agentName, os.TempDir())
	req.CallerMCP = f.server()
	return req
}

func declaredReadOnly(name string) *mcp.Tool {
	tool, _ := callerTool(name, readOnlyAnnotation())
	return tool
}

func argAfter(t *testing.T, args []string, flag string) string {
	t.Helper()
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("no %s in %v", flag, args)
	}
	return args[i+1]
}

func assertNoAgent(t *testing.T, record string) {
	t.Helper()
	for _, suffix := range []string{".args", ".mcp-args"} {
		if _, err := os.Stat(record + suffix); err == nil {
			t.Errorf("an agent process started (%s%s exists)", record, suffix)
		}
	}
}

func shortenCheck(t *testing.T, d time.Duration) {
	t.Helper()
	old := callerMCPCheckLimit
	callerMCPCheckLimit = d
	t.Cleanup(func() { callerMCPCheckLimit = old })
}

func TestRunRestrictedClaudeCarriesTheCallerServerAndItsLockedTools(t *testing.T) {
	f := newCallerFake(t, declaredReadOnly("lookup"), declaredReadOnly("fetch"))
	bin, record := restrictedFake(t, "claude", restrictedClaudeAnswer)
	res, err := restrictedRunner(t, bin, "").RunRestricted(context.Background(), callerRequest(AgentClaude, f))
	if err != nil {
		t.Fatal(err)
	}
	if res.ResultText != "the brief" {
		t.Fatalf("result = %+v", res)
	}
	args := restrictedArgs(t, record, ".args")
	wantAllowed := strings.Join(append(slices.Clone(restrictedReadTools), "mcp__kern_codemode__fetch", "mcp__kern_codemode__lookup"), ",")
	if got := argAfter(t, args, "--allowedTools"); got != wantAllowed {
		t.Errorf("--allowedTools = %q, want %q", got, wantAllowed)
	}
	if got := argAfter(t, args, "--disallowedTools"); got != strings.Join(restrictedDeniedTools, ",") {
		t.Errorf("--disallowedTools = %q", got)
	}
	for _, want := range []string{"--strict-mcp-config", "--permission-prompts", "--setting-sources"} {
		if !slices.Contains(args, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
	data, err := os.ReadFile(argAfter(t, args, "--mcp-config"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Servers map[string]MCPEntry `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Servers) != 1 {
		t.Fatalf("MCP configuration holds %d servers: %s", len(config.Servers), data)
	}
	got := config.Servers["kern_codemode"]
	if got.URL != f.URL || got.Type != "http" || got.Headers["Authorization"] != "Bearer ${CALLER_MCP_TOKEN}" {
		t.Errorf("caller server entry = %+v", got)
	}
	if strings.Contains(string(data), callerToken) || strings.Contains(strings.Join(args, "\n"), callerToken) {
		t.Errorf("the token is on the command line or in the configuration file:\n%v\n%s", args, data)
	}
	env, _ := os.ReadFile(record + ".env")
	if !strings.Contains(string(env), "CALLER_MCP_TOKEN="+callerToken+"\n") {
		t.Errorf("the agent's environment lacks the token:\n%s", env)
	}
	// One check, whose session was closed.
	if _, initializes, listings, deletes := f.counts(); initializes != 1 || listings != 1 || deletes != 1 {
		t.Errorf("initializes %d, listings %d, deletes %d; want one check and its session closed", initializes, listings, deletes)
	}
}

func TestRunRestrictedCodexCarriesTheCallerServerWithOnlyItsLockedTools(t *testing.T) {
	f := newCallerFake(t, declaredReadOnly("lookup"), declaredReadOnly("fetch"))
	bin, record := agenttest.CodexAppServer(t, restrictedCodexScript(agenttest.CodexAppServerScript{
		MCPServers: []string{"tasks", "kern_codemode"},
	}))
	if _, err := restrictedRunner(t, "", bin).RunRestricted(context.Background(), callerRequest(AgentCodex, f)); err != nil {
		t.Fatal(err)
	}
	rec := agenttest.ReadCodexAppServerRecord(t, record)
	args, env := restrictedCodexStart(t, rec, "app-server")
	joined := strings.Join(args, "\n")
	for _, want := range []string{
		`mcp_servers={"tasks"={enabled=false},"restricted_read"={url="http://127.0.0.1:`,
		`,bearer_token_env_var="RESTRICTED_READ_TOKEN"},"kern_codemode"={url="` + f.URL + `",bearer_token_env_var="CALLER_MCP_TOKEN",enabled_tools=["fetch","lookup"],default_tools_approval_mode="approve"}}`,
		`approval_policy="never"`, "features.shell_tool=false", "features.plugins=false", "agents.enabled=false",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q:\n%s", want, joined)
		}
	}
	if strings.Count(joined, `"kern_codemode"=`) != 1 {
		t.Errorf("the inherited server of the caller's name was not replaced:\n%s", joined)
	}
	if strings.Contains(joined, callerToken) {
		t.Errorf("the token is on the command line:\n%s", joined)
	}
	if !slices.Contains(env, "CALLER_MCP_TOKEN="+callerToken) {
		t.Errorf("the agent's environment lacks the token: %v", env)
	}
	if start := codexRecordedThreadStart(t, rec); start.Sandbox != "read-only" || start.ApprovalPolicy != "never" {
		t.Errorf("thread/start = %+v", start)
	}
	if _, initializes, _, deletes := f.counts(); initializes != 1 || deletes != 1 {
		t.Errorf("initializes %d, deletes %d", initializes, deletes)
	}
}

func TestRunRestrictedRefusesToolsNotDeclaredReadOnly(t *testing.T) {
	explicitFalse, _ := callerTool("explicit_false", &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: nil})
	missing, _ := callerTool("missing_annotation", nil)
	other, _ := callerTool("other_annotation", &mcp.ToolAnnotations{IdempotentHint: true})
	for name, tc := range map[string]struct {
		tools []*mcp.Tool
		named []string
	}{
		"missing annotation": {[]*mcp.Tool{declaredReadOnly("lookup"), missing}, []string{"missing_annotation"}},
		"explicit false":     {[]*mcp.Tool{declaredReadOnly("lookup"), explicitFalse}, []string{"explicit_false"}},
		"every offender":     {[]*mcp.Tool{explicitFalse, missing, other}, []string{"explicit_false", "missing_annotation", "other_annotation"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCallerFake(t, tc.tools...)
			bin, record := restrictedFake(t, "claude", restrictedClaudeAnswer)
			_, err := restrictedRunner(t, bin, "").RunRestricted(context.Background(), callerRequest(AgentClaude, f))
			if err == nil {
				t.Fatal("a server with a tool not declared read-only was accepted")
			}
			for _, n := range tc.named {
				if !strings.Contains(err.Error(), n) {
					t.Errorf("error %q does not name %s", err, n)
				}
			}
			if strings.Contains(err.Error(), "lookup") {
				t.Errorf("error names the read-only tool: %v", err)
			}
			assertNoAgent(t, record)
			if _, _, _, deletes := f.counts(); deletes != 1 {
				t.Errorf("deletes = %d, want the check's session closed", deletes)
			}
		})
	}
}

func TestRunRestrictedRefusesAToolNameItCannotCarry(t *testing.T) {
	f := newCallerFake(t, declaredReadOnly("lookup"), declaredReadOnly("a,b"))
	bin, record := restrictedFake(t, "claude", restrictedClaudeAnswer)
	_, err := restrictedRunner(t, bin, "").RunRestricted(context.Background(), callerRequest(AgentClaude, f))
	if err == nil || !strings.Contains(err.Error(), `"a,b"`) {
		t.Fatalf("error = %v", err)
	}
	assertNoAgent(t, record)
}

func TestRunRestrictedRefusesAnEmptyOrUnreachableServer(t *testing.T) {
	empty := newCallerFake(t)
	gone := newCallerFake(t, declaredReadOnly("lookup"))
	for name, tc := range map[string]struct {
		url  string
		want string
	}{
		"no tools":    {empty.URL, "lists no tools"},
		"unreachable": {closedURL(t), "cannot be reached"},
	} {
		t.Run(name, func(t *testing.T) {
			bin, record := restrictedFake(t, "claude", restrictedClaudeAnswer)
			req := restrictedRequestFor(AgentClaude, os.TempDir())
			req.CallerMCP = &CallerMCPServer{Name: "kern_codemode", URL: tc.url, BearerToken: callerToken}
			_, err := restrictedRunner(t, bin, "").RunRestricted(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			assertNoAgent(t, record)
		})
	}
	// A wrong token is a server that cannot be used, not one with no tools.
	req := callerRequest(AgentClaude, gone)
	req.CallerMCP.BearerToken = "wrong"
	bin, record := restrictedFake(t, "claude", restrictedClaudeAnswer)
	if _, err := restrictedRunner(t, bin, "").RunRestricted(context.Background(), req); err == nil {
		t.Fatal("a server that refused the token was accepted")
	}
	assertNoAgent(t, record)
}

// closedURL is the address of a listener that has been closed.
func closedURL(t *testing.T) string {
	t.Helper()
	hs := httptest.NewServer(http.NotFoundHandler())
	u := hs.URL + "/mcp"
	hs.Close()
	return u
}

func TestRunRestrictedEndsAStalledCheckWithinItsLimit(t *testing.T) {
	for _, stall := range []string{"initialize", "tools/list"} {
		t.Run(stall, func(t *testing.T) {
			shortenCheck(t, 400*time.Millisecond)
			f := newCallerFake(t, declaredReadOnly("lookup"))
			f.stall = stall
			bin, record := restrictedFake(t, "claude", restrictedClaudeAnswer)
			started := time.Now()
			_, err := restrictedRunner(t, bin, "").RunRestricted(context.Background(), callerRequest(AgentClaude, f))
			if err == nil || !strings.Contains(err.Error(), "did not answer within") {
				t.Fatalf("error = %v", err)
			}
			if d := time.Since(started); d > 5*time.Second {
				t.Errorf("the check took %s against a limit of 400ms", d)
			}
			assertNoAgent(t, record)
			select {
			case <-f.stalled:
			case <-time.After(5 * time.Second):
				t.Error("the stalled request was left open: the check's connection was not closed")
			}
			if stall == "tools/list" {
				// The session existed, so closing it deletes it.
				if _, _, _, deletes := f.counts(); deletes != 1 {
					t.Errorf("deletes = %d, want the check's session closed", deletes)
				}
			}
		})
	}
}

func TestRunRestrictedEndsTheCheckWhenTheCallersContextIsCancelled(t *testing.T) {
	for _, stall := range []string{"initialize", "tools/list"} {
		t.Run(stall, func(t *testing.T) {
			f := newCallerFake(t, declaredReadOnly("lookup"))
			f.stall = stall
			bin, record := restrictedFake(t, "claude", restrictedClaudeAnswer)
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(300*time.Millisecond, cancel)
			started := time.Now()
			_, err := restrictedRunner(t, bin, "").RunRestricted(ctx, callerRequest(AgentClaude, f))
			if err == nil || !strings.Contains(err.Error(), "cancelled") {
				t.Fatalf("error = %v", err)
			}
			if d := time.Since(started); d > 3*time.Second {
				t.Errorf("the check took %s to notice the cancellation", d)
			}
			assertNoAgent(t, record)
			select {
			case <-f.stalled:
			case <-time.After(5 * time.Second):
				t.Error("the stalled request was left open: the check's connection was not closed")
			}
		})
	}
}

func TestRunRestrictedRefusesACallerServerNameBeforeAnyNetworkCall(t *testing.T) {
	f := newCallerFake(t, declaredReadOnly("lookup"))
	for _, name := range []string{ReadServerName, "Restricted_Read", "", "kern-codemode", "kern codemode", "1kern", "_kern", "kern.codemode", `kern"`, strings.Repeat("a", 33)} {
		t.Run(name, func(t *testing.T) {
			bin, record := restrictedFake(t, "claude", restrictedClaudeAnswer)
			req := callerRequest(AgentClaude, f)
			req.CallerMCP.Name = name
			if _, err := restrictedRunner(t, bin, "").RunRestricted(context.Background(), req); err == nil {
				t.Fatalf("name %q was accepted", name)
			}
			assertNoAgent(t, record)
		})
	}
	for _, name := range []string{"kern_codemode", "a", "Codemode2", "a" + strings.Repeat("b", 31)} {
		if err := validateCallerMCP(&CallerMCPServer{Name: name, URL: f.URL}); err != nil {
			t.Errorf("name %q refused: %v", name, err)
		}
	}
	for _, url := range []string{"", "not a url", "ftp://127.0.0.1/mcp", "http:///mcp", "http://user:pw@127.0.0.1/mcp", "stdio:server"} {
		req := callerRequest(AgentClaude, f)
		req.CallerMCP.URL = url
		if err := validateRestrictedRequest(req); err == nil {
			t.Errorf("URL %q was accepted", url)
		}
	}
	if requests, _, _, _ := f.counts(); requests != 0 {
		t.Errorf("%d requests reached the server for refused requests", requests)
	}
}

func TestRunRestrictedRefusesABackendThatCannotCarryTheCallerServer(t *testing.T) {
	f := newCallerFake(t, declaredReadOnly("lookup"))
	openCode, openCodeRecord := restrictedOpenCodeFake(t, "", restrictedOpenCodeAnswer)
	pi, piRecord := restrictedPiFake(t, restrictedPiAnswer)
	claude, claudeRecord := restrictedFake(t, "claude", restrictedClaudeAnswer)
	r := &Runner{ClaudeBin: claude, OpenCodeBin: openCode, PiBin: pi, SessionsDir: t.TempDir(), EnvironmentPrefix: "TASK_"}
	for name, profile := range map[string]Profile{
		"opencode primary": {Name: "r", Agent: AgentOpenCode},
		"pi primary":       {Name: "r", Agent: AgentPi},
		"opencode fallback": {Name: "r", Agent: AgentClaude,
			Fallback: &Profile{Name: "r", Agent: AgentOpenCode}},
		"pi fallback": {Name: "r", Agent: AgentClaude,
			Fallback: &Profile{Name: "r", Agent: AgentClaude, Fallback: &Profile{Name: "r", Agent: AgentPi}}},
	} {
		t.Run(name, func(t *testing.T) {
			req := callerRequest(profile.Agent, f)
			req.Profile = profile
			_, err := r.RunRestricted(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), "cannot carry a caller MCP server") {
				t.Fatalf("error = %v", err)
			}
		})
	}
	for _, record := range []string{openCodeRecord, piRecord, claudeRecord} {
		assertNoAgent(t, record)
	}
	if requests, _, _, _ := f.counts(); requests != 0 {
		t.Errorf("%d requests reached the server for refused requests", requests)
	}
}

func TestRunRefusesACallerServer(t *testing.T) {
	f := newCallerFake(t, declaredReadOnly("lookup"))
	bin, record := restrictedFake(t, "claude", restrictedClaudeAnswer)
	req := callerRequest(AgentClaude, f)
	req.Grants = &Grants{}
	_, err := restrictedRunner(t, bin, "").Run(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "CallerMCP") {
		t.Fatalf("error = %v", err)
	}
	assertNoAgent(t, record)
	if requests, _, _, _ := f.counts(); requests != 0 {
		t.Errorf("%d requests reached the server", requests)
	}
}

func TestRunRestrictedKeepsItsOtherRefusalsWithACallerServer(t *testing.T) {
	f := newCallerFake(t, declaredReadOnly("lookup"))
	bin, record := restrictedFake(t, "claude", restrictedClaudeAnswer)
	r := restrictedRunner(t, bin, "")
	for name, mutate := range map[string]func(*Request){
		"HostMCP":     func(req *Request) { req.HostMCP = &HostMCP{Name: "tasks"} },
		"Profile.MCP": func(req *Request) { req.Profile.MCP = map[string]MCPEntry{"tasks": {Command: "tasks"}} },
		"fallback MCP": func(req *Request) {
			req.Profile.Fallback = &Profile{Name: "r", MCP: map[string]MCPEntry{"tasks": {Command: "tasks"}}}
		},
		"allowed tools": func(req *Request) { req.Profile.AllowedTools = []string{"mcp__other"} },
	} {
		t.Run(name, func(t *testing.T) {
			req := callerRequest(AgentClaude, f)
			mutate(&req)
			if _, err := r.RunRestricted(context.Background(), req); err == nil {
				t.Fatal("request accepted")
			}
			assertNoAgent(t, record)
		})
	}
	if requests, _, _, _ := f.counts(); requests != 0 {
		t.Errorf("%d requests reached the server for refused requests", requests)
	}
}

func TestRunRestrictedLocksTheListedToolsForEveryAttempt(t *testing.T) {
	f := newCallerFake(t, declaredReadOnly("lookup"))
	// After the check lists the tools, the server adds one.
	var once sync.Once
	f.srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if method == "tools/list" {
				once.Do(func() {
					tool, h := callerTool("added_later", readOnlyAnnotation())
					go f.srv.AddTool(tool, h)
				})
			}
			return res, err
		}
	})
	limited, limitedRecord := restrictedFake(t, "claude", `echo '{"type":"result","subtype":"error","is_error":true,"result":"rate limit reached","session_id":"s0"}'`)
	answering, answerRecord := agenttest.CodexAppServer(t, restrictedCodexScript(agenttest.CodexAppServerScript{}))
	req := callerRequest(AgentClaude, f)
	req.Profile.Fallback = &Profile{Name: "reviewer", Agent: AgentCodex, Model: "gpt-last"}
	res, err := restrictedRunner(t, limited, answering).RunRestricted(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Agent != AgentCodex {
		t.Fatalf("the fallback did not answer: %+v", res)
	}
	// The check ran once for the turn, not once per attempt.
	if _, initializes, listings, _ := f.counts(); initializes != 1 || listings != 1 {
		t.Errorf("initializes %d, listings %d; want one check", initializes, listings)
	}
	// The server now lists both tools.
	deadline := time.Now().Add(5 * time.Second)
	for {
		session, err := mcp.NewClient(&mcp.Implementation{Name: "probe", Version: "1"}, nil).Connect(context.Background(),
			&mcp.StreamableClientTransport{Endpoint: f.URL, HTTPClient: &http.Client{Transport: &callerMCPTransport{token: callerToken, base: http.DefaultTransport.(*http.Transport).Clone()}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		list, err := session.ListTools(context.Background(), nil)
		_ = session.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(list.Tools) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the server never listed the added tool")
		}
		time.Sleep(20 * time.Millisecond)
	}
	primary := restrictedArgs(t, limitedRecord, ".args")
	if got := argAfter(t, primary, "--allowedTools"); !strings.HasSuffix(got, ",mcp__kern_codemode__lookup") || strings.Contains(got, "added_later") {
		t.Errorf("primary --allowedTools = %q", got)
	}
	rec := agenttest.ReadCodexAppServerRecord(t, answerRecord)
	args, _ := restrictedCodexStart(t, rec, "app-server")
	joined := strings.Join(args, "\n")
	if !strings.Contains(joined, `enabled_tools=["lookup"]`) || strings.Contains(joined, "added_later") {
		t.Errorf("fallback tools are not the locked list:\n%s", joined)
	}
}
