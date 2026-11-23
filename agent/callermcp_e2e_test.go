package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest"
	"github.com/kpenfound/agentcore/mcphost"
	"github.com/kpenfound/agentcore/vcs"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// callerAgentEnv set to 1 makes the test binary a stand-in for claude that
// does what a restricted turn's agent does with its configuration: it reads
// the --mcp-config file, expands the token reference from its environment,
// calls the first locked tool of --allowedTools on the server and reports the
// server's answer as its result. It runs from init because the test binary's
// own TestMain is in the internal test package.
const callerAgentEnv = "AGENTCORE_TEST_CALLER_AGENT"

func init() {
	if os.Getenv(callerAgentEnv) == "1" {
		os.Exit(runCallerAgent(os.Args[1:]))
	}
}

func runCallerAgent(args []string) int {
	value := func(flag string) string {
		if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	data, err := os.ReadFile(value("--mcp-config"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var config struct {
		Servers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &config); err != nil || len(config.Servers) != 1 {
		fmt.Fprintln(os.Stderr, "mcp config:", err, string(data))
		return 1
	}
	var server, tool string
	for _, allowed := range strings.Split(value("--allowedTools"), ",") {
		if parts := strings.Split(allowed, "__"); len(parts) == 3 && parts[0] == "mcp" {
			server, tool = parts[1], parts[2]
			break
		}
	}
	entry, ok := config.Servers[server]
	if !ok {
		fmt.Fprintln(os.Stderr, "no server", server, "in", string(data))
		return 1
	}
	headers := map[string]string{}
	for k, v := range entry.Headers {
		headers[k] = os.ExpandEnv(v)
	}
	ctx := context.Background()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "fake-claude", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   entry.URL,
		HTTPClient: &http.Client{Transport: headerTransport(headers)},
	}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = session.Close() }()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"question": "what is the answer"}})
	if err != nil || res.IsError || len(res.Content) == 0 {
		fmt.Fprintln(os.Stderr, "call:", err, res)
		return 1
	}
	text := res.Content[0].(*mcp.TextContent).Text
	out, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text, "session_id": "s-e2e", "num_turns": 2})
	fmt.Println(string(out))
	return 0
}

type headerTransport map[string]string

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

type lookupInput struct {
	Question string `json:"question"`
}

// A Kern-shaped turn: the caller serves read-only tools on an mcphost lease,
// hands the endpoint to RunRestricted, and the agent calls a tool and gets
// the server's answer. The lease stays the caller's.
func TestRunRestrictedReachesAServerOnAnMCPHostLease(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "codemode", Version: "1"}, nil)
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(srv, &mcp.Tool{Name: "codemode", Description: "answers a question", Annotations: readOnly},
		func(_ context.Context, _ *mcp.CallToolRequest, in lookupInput) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "the lease answers: " + in.Question}}}, nil, nil
		})
	endpoint, lease, err := mcphost.Start(context.Background(), srv)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	claude := agenttest.Script(t, "claude", callerAgentEnv+`=1 exec '`+self+`' "$@"`)
	runner := &agent.Runner{ClaudeBin: claude, SessionsDir: t.TempDir()}
	res, err := runner.RunRestricted(context.Background(), agent.Request{
		Name:      "kern-turn",
		Profile:   agent.Profile{Name: "kern", Agent: agent.AgentClaude},
		Workspace: vcs.Directory(t.TempDir()),
		Prompt:    "look it up",
		CallerMCP: &agent.CallerMCPServer{Name: "kern_codemode", URL: endpoint.URL, BearerToken: endpoint.Token},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ResultText != "the lease answers: what is the answer" {
		t.Errorf("result = %q", res.ResultText)
	}
	// agentcore never closed the lease.
	session, err := lease.Connect(context.Background(), mcp.Implementation{Name: "after", Version: "1"})
	if err != nil {
		t.Fatalf("the lease was closed by the turn: %v", err)
	}
	_ = session.Close()
}
