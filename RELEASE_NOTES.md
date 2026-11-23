# Release notes

## Unreleased

`Request.CallerMCP`, a `*CallerMCPServer{Name, URL, BearerToken}`, gives a
`Runner.RunRestricted` turn one MCP server the caller already serves over
streamable HTTP, for example an `mcphost` lease's endpoint. Before any agent
process starts, `RunRestricted` connects with the token and lists the server's
tools, once per request, within `CallerMCPCheckTimeout` (20 seconds) and the
caller's context, and closes the check's session on every path. It refuses the
request, naming the tools, when a tool lacks `readOnlyHint: true`, and when the
server is unreachable, silent or lists no tools. The listed names are the
turn's allowed tools for the server, for every fallback attempt too. `Name`
must match `^[A-Za-z][A-Za-z0-9_]{0,31}$` and differ from `restricted_read`;
a bad name or URL is refused before any network call. Claude holds the server
in its strict MCP configuration and allows `mcp__<name>__<tool>` for the locked
tools; Codex enables the server beside `restricted_read` with only the locked
tools. The token travels in `CALLER_MCP_TOKEN`, never on a command line.
`RestrictedCapabilities.CallerMCP` declares which backends carry the server:
Claude and Codex do, and a request whose profile or fallback chain reaches
OpenCode or Pi is refused before anything starts. `Run` and `Complete` refuse
the field. agentcore checks the declarations; the caller answers for the tools
actually being read-only. A request with `CallerMCP` nil is unchanged.

A Codex turn whose `Profile.Skills` is non-empty receives those skills.
`Runner.Skills` prepares them, and the session's `codex app-server` gets one
`skills/extraRoots/set` request, with the absolute `skills` directory of each
prepared plugin as `extraRoots`, after `initialize` and the `initialized`
notification and before `thread/start` or `thread/resume`. This holds for
ordinary, `ToolsAll`, held, restricted and resumed turns. The turn fails
before any thread starts, with an error that names skills, when the runner has
no `SkillPreparer`, preparation fails, a prepared plugin has no `skills`
directory, the server answers the request with an error (a Codex without
`skills/extraRoots/set` answers "method not found") or the server ends before
answering. A profile without skills prepares nothing and sends no request.
Claude receives its prepared skills as `--plugin-dir`; opencode and pi receive
none.

`Runner.RunRestricted` accepts `Profile.Skills` on a Codex primary or fallback
profile and keeps `Runner.Skills` and `Runner.SkillMountDirs` for such a turn.
Skills on a claude, opencode or pi profile are rejected, and every other
restriction applies to a Codex turn with skills as to any other.
`agenttest.CodexAppServerScript` has `SkillsResult` and `SkillsError` to
script the answer to `skills/extraRoots/set`, and
`ExitBefore: "skills/extraRoots/set"` to end the fake before it answers.

`CompleterOptions.IdleTimeout`, when above zero, removes a `Completer`'s kept
sandbox once no call has taken it for that long; the next call creates a
fresh one. Sandboxes are VMs that hold memory while kept, so a caller with
bursts of completions keeps them warm through a burst and frees them between
bursts. A sandbox in use is never removed for being idle, and `Close` stops
the reaper and waits for a removal it had begun. Zero keeps the previous
behaviour: sandboxes stay until `Close`.

## v1.2.3

`Runner.NewCompleter` returns a `Completer`, whose `Complete` has
`Runner.Complete`'s contract but runs each call with `sbx exec` in a sandbox
it keeps, creating at most `CompleterOptions.Size` of them, instead of
creating and removing one per call: one registry resolution of the kit per
sandbox rather than per call. Each call gets a new empty working directory
inside the sandbox's workspace, removed when it ends. A sandbox is replaced
after a call that did not report the end of its turn (a missing sandbox, a
timeout, a cancellation) and kept after any model outcome. `Close` removes
them; `ErrCompleterClosed` refuses calls after it. A `sbx create` refused for
the registry's pull rate limit matches `ErrRegistryRateLimited`, for every
session. `agenttest.Sbx` has `rate-limit-create` and `fail-exec` markers and
records each `exec --interactive` in `sbx-exec.txt`.

`Runner.Complete` runs one tool-free `claude -p` turn in a Docker Sandbox of
its own and returns a `Completion`: the text, the schema-checked
`structured_output` as raw JSON, the token counts, the reported cost, the
answering model, the turn's duration and the time the sandbox took to start.
The call takes a `CompletionRequest` with a required system prompt that
replaces Claude Code's own, one prompt, an optional JSON schema
(`--json-schema`), an optional output token limit
(`CLAUDE_CODE_MAX_OUTPUT_TOKENS`), a timeout, a cost cap and the sandbox
template, CPU and memory settings. The sandbox is created for the call and
removed however it ends, and holds one new empty directory the call removes.
Claude gets no tools, no setting sources, no skills and an empty strict MCP
configuration, and no agent credential is forwarded: the sandbox's proxy
supplies the stored login. The timeout is one deadline over sandbox startup
and the turn, and the caller's context applies from the start. Failures are
one of four kinds, in order: `*UsageLimitError` (with the reset time),
the context's error, `*NoStructuredOutputError` (`ErrNoStructuredOutput`),
and any other failure naming the session, subtype and result text.

`Runner.CheckSandbox` runs `sbx ls` under `Runner.SbxCheckTimeout` (default 15
seconds) and returns nil when `sbx` answers. Otherwise the error names the
`sbx` executable and carries sbx's message. It creates no sandbox.

`agenttest.Sbx` answers `ls`, with `fail-ls` and `hang-ls` markers, and has a
`hang-create` marker. The `sbx` commands the runner starts (create, policy,
rm) are released 10 seconds after their context ends even when a child of
theirs still holds their output. `Run`, `RunRestricted`, sandbox sessions and
their result files are unchanged.

`Mount.EmptyRepository` (a `bool`, false by default) marks a writable mount
whose top holds an empty git repository, as `git init --quiet --template=`
leaves it. A `sandbox = "sbx"` session admits a marked mount without
`Grants.VCS`; every other sandbox refuses it with `ErrUnsupported`. The
repository is checked at `Prepare` and before every turn, and one that holds
objects, refs, `packed-refs`, hooks, a symbolic link, or a remote, include or
`core.hooksPath` in its config is refused with `ErrNotGranted`. VCS stays
denied to the turn. The mark shows on the mount in `Policy().Mounts` and
counts in the comparison of a session's grants and of each turn's mounts with
its policy. A `Mount` marshalled to JSON without the mark has the same form
as before.

`Profile.SandboxCPUs` (an `int`) and `Profile.SandboxMemory` (a string such as
`512m` or `4g`) size the sandbox of a `sandbox = "sbx"` session. When set, they
reach `sbx create` as `--cpus <n>` and `--memory <value>`, for `Runner.Run`
sessions and `NewSbx` turns. Unset (zero, empty) leaves sizing to sbx, with the
same `sbx create` arguments as before. `Profile.Validate` refuses a negative
count, a memory value that is not a positive integer and `m` or `g`, and
either setting on a sandbox other than `sbx`; the error names the field, and a
refused profile runs no `sbx` command. Each `Fallback` profile carries its own
values, applied when a caller runs it. `CheckSandboxCPUs` and
`CheckSandboxMemory` expose the format checks.

Sandbox sessions create their sandbox with `sbx create --no-progress`. Docker
Sandboxes v0.50 removed `--quiet`, so earlier releases fail every `sbx`
session's create on it.

Jev judge defaults use TypeSafe's own API at `https://api.typesafe.ai`, model
`jev-latest`, and the key variable `TYPESAFE_API_KEY`, with a 10-second timeout.

Consumers that relied on the previous OpenRouter defaults, such as Osmia,
must now set all three OpenRouter connection settings explicitly:

```go
settings := jev.DefaultSettings()
settings.URL = "https://openrouter.ai/api"
settings.Model = "~typesafe/jev-latest"
settings.APIKeyEnv = "OPENROUTER_API_KEY"
```

Set `Enabled` to use the judge. Requests go to
`https://openrouter.ai/api/v1/systemone`, with the bearer credential read from
`OPENROUTER_API_KEY` at request time.
