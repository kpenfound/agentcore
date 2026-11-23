package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CallerMCPServer is one MCP server the caller already serves over streamable
// HTTP and hands to RunRestricted through Request.CallerMCP. agentcore starts
// no process for it and never stops or closes it: the server, and the lease or
// listener behind it, belong to the caller. An mcphost.Endpoint maps onto it
// field by field (URL from Endpoint.URL, BearerToken from Endpoint.Token).
//
// RunRestricted lists the server's tools before any agent starts and refuses
// the request unless every one of them is declared read-only. The names listed
// then are the only tools of the server the turn may call.
type CallerMCPServer struct {
	// Name is the MCP server name the agent reaches the server by. It must
	// match callerMCPNamePattern, ^[A-Za-z][A-Za-z0-9_]{0,31}$, which is valid
	// as a Claude server name and as a Codex mcp_servers key, and must not be
	// ReadServerName.
	Name string
	// URL is the streamable-HTTP endpoint the caller serves: an http or
	// https URL with a host and without credentials of its own.
	URL string
	// BearerToken is the token the server requires, presented as
	// "Authorization: Bearer". The agent receives it through its environment,
	// never its command line.
	BearerToken string
}

const (
	// CallerMCPCheckTimeout bounds RunRestricted's pre-launch check of a
	// CallerMCP server: connecting, initializing and listing every tool.
	// The check also ends when the caller's context does.
	CallerMCPCheckTimeout = 20 * time.Second

	// callerMCPTokenEnv carries the caller server's bearer token to the
	// agent, the way readServerTokenEnv carries the read server's.
	callerMCPTokenEnv = "CALLER_MCP_TOKEN"

	// callerMCPCloseTimeout bounds the request that ends the check's session,
	// which the check's own deadline no longer covers.
	callerMCPCloseTimeout = 5 * time.Second
	// callerMCPMaxTools and callerMCPMaxPages bound what the check reads from
	// one server.
	callerMCPMaxTools = 256
	callerMCPMaxPages = 32
)

var (
	// callerMCPCheckLimit is CallerMCPCheckTimeout; tests shorten it.
	callerMCPCheckLimit = CallerMCPCheckTimeout

	callerMCPNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,31}$`)
	// callerMCPToolPattern is a tool name Claude's mcp__<server>__<tool> and
	// Codex's enabled_tools both carry unchanged, and that cannot split the
	// comma-separated --allowedTools list.
	callerMCPToolPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// validateCallerMCP refuses a server that cannot be carried, before any
// network call.
func validateCallerMCP(s *CallerMCPServer) error {
	switch {
	case !callerMCPNamePattern.MatchString(s.Name):
		return fmt.Errorf("restricted session: caller MCP server name %q must match %s", s.Name, callerMCPNamePattern)
	case strings.EqualFold(s.Name, ReadServerName):
		return fmt.Errorf("restricted session: caller MCP server name %q is the read server's", s.Name)
	}
	u, err := url.Parse(s.URL)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "":
		return fmt.Errorf("restricted session: caller MCP server %q needs an http or https URL with a host", s.Name)
	case u.User != nil:
		return fmt.Errorf("restricted session: caller MCP server %q URL carries credentials; pass the token as BearerToken", s.Name)
	case strings.ContainsAny(s.BearerToken, "\r\n\x00"):
		return fmt.Errorf("restricted session: caller MCP server %q bearer token is not a valid header value", s.Name)
	}
	return nil
}

// checkCallerMCP connects to the caller's server, lists its tools and returns
// their names, the locked list of the turn. It refuses a server that lists no
// tool, a tool that is not declared read-only (annotation readOnlyHint true;
// a missing annotation is not) and a tool whose name cannot be carried
// unchanged. It ends within callerMCPCheckLimit and when ctx does, and its
// client session is closed on every path.
func checkCallerMCP(ctx context.Context, s CallerMCPServer) ([]string, error) {
	fail := func(format string, a ...any) ([]string, error) {
		return nil, fmt.Errorf("restricted session: caller MCP server %q "+format, append([]any{s.Name}, a...)...)
	}
	checkCtx, cancel := context.WithTimeout(ctx, callerMCPCheckLimit)
	defer cancel()
	// ended says why the check stopped when its context did.
	ended := func(err error) error {
		switch {
		case ctx.Err() != nil:
			return fmt.Errorf("check cancelled: %w", context.Cause(ctx))
		case checkCtx.Err() != nil:
			return fmt.Errorf("did not answer within %s", callerMCPCheckLimit)
		}
		return err
	}

	transport := &callerMCPTransport{token: s.BearerToken, base: http.DefaultTransport.(*http.Transport).Clone()}
	defer transport.base.CloseIdleConnections()
	client := mcp.NewClient(&mcp.Implementation{Name: "agentcore-restricted-check", Version: "1"}, nil)
	session, err := client.Connect(checkCtx, &mcp.StreamableClientTransport{
		Endpoint:             s.URL,
		HTTPClient:           &http.Client{Transport: transport},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		return fail("cannot be reached: %v", ended(err))
	}
	defer func() { _ = session.Close() }()

	var tools []*mcp.Tool
	cursor := ""
	for page := 0; ; page++ {
		if page == callerMCPMaxPages {
			return fail("lists more than %d pages of tools", callerMCPMaxPages)
		}
		res, err := session.ListTools(checkCtx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return fail("could not list its tools: %v", ended(err))
		}
		tools = append(tools, res.Tools...)
		if len(tools) > callerMCPMaxTools {
			return fail("lists more than %d tools", callerMCPMaxTools)
		}
		if cursor = res.NextCursor; cursor == "" {
			break
		}
	}
	if len(tools) == 0 {
		return fail("lists no tools")
	}
	var names, notReadOnly, unusable []string
	for _, tool := range tools {
		switch {
		case !callerMCPToolPattern.MatchString(tool.Name):
			unusable = append(unusable, fmt.Sprintf("%q", tool.Name))
		case tool.Annotations == nil || !tool.Annotations.ReadOnlyHint:
			notReadOnly = append(notReadOnly, tool.Name)
		}
		names = append(names, tool.Name)
	}
	if len(notReadOnly) > 0 {
		slices.Sort(notReadOnly)
		return fail("lists tools not declared read-only (readOnlyHint true): %s", strings.Join(notReadOnly, ", "))
	}
	if len(unusable) > 0 {
		slices.Sort(unusable)
		return fail("lists tools whose names do not match %s: %s", callerMCPToolPattern, strings.Join(unusable, ", "))
	}
	slices.Sort(names)
	return names, nil
}

// callerMCPTransport presents the bearer token on every request of the check
// and bounds the request that deletes its session: the SDK sends that one on
// a context the check's deadline does not reach.
type callerMCPTransport struct {
	token string
	base  *http.Transport
}

func (t *callerMCPTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if t.token != "" {
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	if r.Method != http.MethodDelete {
		return t.base.RoundTrip(r)
	}
	ctx, cancel := context.WithTimeout(r.Context(), callerMCPCloseTimeout)
	resp, err := t.base.RoundTrip(r.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	defer c.cancel()
	return c.ReadCloser.Close()
}

// callerMCPEntry is the server as Claude's MCP configuration holds it: the
// token is an environment reference, never the value.
func callerMCPEntry(s *CallerMCPServer) MCPEntry {
	e := MCPEntry{Type: "http", URL: s.URL}
	if s.BearerToken != "" {
		e.BearerTokenEnv = callerMCPTokenEnv
	}
	return e
}

// callerMCPEnv is the environment the agent is given the token through.
func callerMCPEnv(s *CallerMCPServer) []envVar {
	if s.BearerToken == "" {
		return nil
	}
	return []envVar{{callerMCPTokenEnv, s.BearerToken}}
}

// callerMCPAllowedTools names the locked tools the way Claude does.
func callerMCPAllowedTools(server string, tools []string) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = "mcp__" + server + "__" + t
	}
	return out
}

var errCallerMCPNotLocked = errors.New("restricted session: caller MCP server has no locked tool list")

// checkCallerMCPTurn is runTurn's own guard on the field: only a restricted
// turn takes it, only with the list RunRestricted locked, and only on a
// backend that declares it carries it.
func checkCallerMCPTurn(req Request, restricted bool) error {
	if req.CallerMCP == nil {
		return nil
	}
	if !restricted {
		return errors.New("session: Request.CallerMCP is only accepted by RunRestricted")
	}
	if len(req.callerTools) == 0 {
		return errCallerMCPNotLocked
	}
	be, err := backendFor(req.Profile.Agent)
	if err != nil {
		return err
	}
	if be.Restricted == nil || !be.Restricted.CallerMCP {
		return fmt.Errorf("restricted session: agent %q cannot carry a caller MCP server", be.Name)
	}
	return nil
}

// codexCallerMCPEntry is the caller's server as an entry of codex's
// mcp_servers override: only the locked tools enabled, the token read from the
// environment by name, and the tools approved, since the session was granted
// the server, only tools declared read-only are on it, and approvalPolicy
// "never" leaves no one to ask.
func codexCallerMCPEntry(s *CallerMCPServer, tools []string) string {
	entry := "{url=" + codexValue(s.URL)
	if s.BearerToken != "" {
		entry += ",bearer_token_env_var=" + codexValue(callerMCPTokenEnv)
	}
	return codexValue(s.Name) + "=" + entry +
		",enabled_tools=" + codexList(tools) +
		",default_tools_approval_mode=" + codexValue("approve") + "}"
}
