# agentcore — notes for coding agents

agentcore (`github.com/kpenfound/agentcore`) is a Go module that runs headless
coding-agent sessions (claude, codex, opencode, pi) and supplies what a factory
built on them needs: the execution boundary, an MCP host, a review pipeline,
workspaces and operational primitives. It holds no tracker, configuration file
or scheduling policy: callers supply those.
[busybees](https://github.com/kpenfound/busybees) is one caller; read
`README.md` for the contracts before changing one.

## Build and test

- Everything builds and tests through Dagger. `dagger check` runs the official
  `github.com/dagger/go` module's checks (tests and generators). Run it before
  committing. `dagger check` is the only validation a pull request may report
  as done.
- The Go tests and generators run in `agentcore-dev`'s `test-runtime`
  (`.dagger/modules/agentcore-dev`, the `go` module's `base` in `dagger.toml`):
  `golang:1.26-bookworm` with a pinned, checksummed `jj` release. It sets
  `AGENTCORE_REQUIRE_JJ`, which turns a missing `jj` into a failure in the jj
  tests; a container without either skips them. A tool a test needs goes there.
- Never run `go test` on the host, in any role and for any purpose: iterating,
  a single test, a mutation check and reproducing a flake included. Tests start
  processes that leak onto the machine they run on, so they run only inside
  Dagger. `gofmt`, `go build` and `go vet` are fine on the host; `go vet ./...`
  type-checks test files without running them. `CONTRIBUTING.md` has the
  one-package recipe.
- Tests must never call the real `claude`, `codex`, `opencode`, `pi`, `docker`
  or `sbx`. `agent/agenttest` fakes them with shell scripts or the test binary
  itself, and `agent/agentbin.Resolve`, which every session goes through,
  refuses any other executable from a test binary (`testing.Testing()`), so a
  test that forgets its fake fails with `ErrRealAgent` instead of running the
  real agent on the host. Git is real: tests create repositories under
  `t.TempDir()`.
- There is no CI workflow: `dagger check` is the gate, and it runs on pull
  requests through Dagger Cloud Checks.

## Layout

- `agent` — the headless session runner: backend commands
  and streams, sandbox/container execution, timeout/cancellation, result/outcome
  files and interruption inspection. `grants.go` is the capability contract every
  request carries (`Grants`: env allowlist, tools, ro/rw mounts, VCS; a grant naming built-in tools is held to them
  where the backend's `WritableTools` in `backends.go` declares the placement supported, opencode through its generated
  `agentcore-granted` agent checked with `opencode debug config` where the turn runs (`probe.go`), after the search for the custom tools opencode would load that refuses any held opencode turn (`opencode_customtools.go`), codex through configuration derived from the grant and checked with `codex features list`, `codex mcp list` and the app server's `config/read` there (`codex_tools.go`), and refused elsewhere) and the
  `Boundary` that verifies it before launch (`HostBoundary`, `ContainerBoundary`, and `SandboxBoundary` in `sbx.go`,
  the Docker Sandbox mode `sandbox = "sbx"`: `sbx create <agent>` with the binds as workspaces, `sbx exec` around the agent's
  command, `sbx rm` at the end, the agent's credential left to the sbx proxy; `sbxdagger.go`, the opt-in Dagger CLI and
  host engine forward, `Profile.Dagger` and `Grants.DaggerEngine`; `sbxhost.go`, `Grants.HostServers`: a caller's own MCP servers on the host's loopback, each port allowed for one sandbox alone). `confine.go` is the host's
  confined mode (`Profile.Confine`): the operating system holds the process to its mounts and `SystemPaths` through a
  `Confiner`, Landlock on Linux (`confine_linux.go`), Seatbelt on macOS (`confine_seatbelt.go`: the profile and the
  `sandbox-exec` start, untagged so the gate tests them; `confine_darwin.go` selects it), `ErrUnsupported` where there
  is none; the Landlock enforcement tests skip on a kernel without Landlock and run in the VM `CONTRIBUTING.md`
  describes, and Seatbelt enforcement is checked by hand with the recipe there. `enforce.go` is the runner an embedder
  takes a held turn from (`Enforcer`: `NewHostNone`, `NewHostClaude`, `NewContainer`, `NewSbx`; `Prepare(ctx, grants)` → `Session`
  with `Policy`, `Run` and `Release`): host kinds are confined turns, a container session binds stand-ins over its
  image's VCS executables (`containermask.go`, `ContainerBoundary.Masks`), and `Run` refuses a turn the reported policy
  does not describe (`ErrPolicyChanged`). An ordinary caller's sessions go through `Runner.Run`, and a read-only
  analysis turn through `Runner.RunRestricted`.
- `agent/agentbin` — `Resolve`, the one place a session turns an agent name into
  the executable it runs, and the guard that keeps a test binary from running a
  real one.
- `agent/agenttest` — fake agents, Docker, sbx and a host MCP server for tests,
  here and in callers. `agent/agenttest/enforcertest` is an `Enforcer` that
  starts no process.
- `agent/procs` — PID files, process-table inspection and container cleanup.
  The caller supplies its command markers and container label.
- `mcphost` — generic MCP server metadata, typed role-scoped registration,
  in-memory clients, stdio/HTTP lifecycle, per-turn endpoints (`lease.go`:
  `Start`/`StartOn` → `Endpoint` and `Lease`, `StartMemory` for tests) and
  caller-supplied outcome reporting.
- `ops` — retry decisions, ledger storage and budget signals, capacity and
  degraded episodes, the event bus, fair shared slots and tick/wake primitives.
  The caller supplies configuration, work mapping, logging and escalation policy.
- `review` — review pipeline over supplied context and diff:
  distillation, size-based concurrent angles, deterministic judge, findings,
  reviewer-note filtering and artifact persistence. The caller supplies a
  reference type (display, URL and opaque rule scope), agent implementations
  and profiles, comparison policy and working directories, and keeps
  acquisition, publication, notes storage and triage.
- `skills` — skill repositories by git URL, laid out as plugin directories
  (`Manager`; `Manager.Git` is replaced in tests with a copy of a fixture
  directory, so every supported layout is exercised offline).
- `vcs` — workspace directory, optional VCS mounts and provider lifecycle; no
  git discovery. `replay.go` is the optional `Replayer` (replay `OldBase..Head` onto `Onto` without
  moving a branch, resumable after a conflict and a restart), implemented by
  `vcs/git` (cherry-pick) and `vcs/jj` (`jj duplicate`). `sign.go` is the optional
  `Signer` (signed commits from reviewed trees with explicit parents and messages, the
  repository's identity and key, no unsigned fallback, recorded under an operation id
  and kept under `refs/core-sign/` until `Forget`), implemented by `vcs/git`
  (`git commit-tree -S`) for any git store, a Jujutsu one included.
- `work` — opaque work keys, caller tags and collision-resistant filenames.

## Conventions

- Nothing here imports a caller. Policy that belongs to one caller stays in
  that caller; this module takes it as a parameter or an interface.
- A change that breaks callers' code or on-disk formats is called out in the
  pull request and the release notes: callers read ledgers, review artifacts
  and session directories written by earlier versions.

## Documentation

- Docs describe the current state. They never describe a change from previous
  behaviour: no "now", no "no longer", no "as of v0.2".
- Docs instruct, they do not sell. No marketing language, no piled-up adjectives.
- Prefer an example over an extended explanation.
- `README.md` documents the contracts callers build on. `CONTRIBUTING.md`
  holds testing instructions, style rules, the contributor workflow and
  releasing.
- The `unslop` writing skill applies to every markdown file.
