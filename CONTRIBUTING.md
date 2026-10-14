# Contributing

## Package layout

```
agent/                         session runner, backends, sandbox and result artifacts
agent/agentbin/                fake-executable guard
agent/agenttest/               shared fake agents, Docker, sbx and host MCP server
agent/agenttest/enforcertest/  an agent.Enforcer that starts no process
agent/procs/                   finding and stopping sessions (processes and containers)
mcphost/                       MCP host, typed role-scoped registration, lifecycle and outcome tool
ops/                           retry, ledger/budgets, pause/degraded signals, events, slots and wakes
review/                        brief, concurrent angles, judge, findings, filtering and artifacts over supplied context and diff
skills/                        skill repositories by git URL, laid out as plugin directories
vcs/                           workspace contract and provider lifecycle, replaying revisions, signing commits; no git discovery
work/                          opaque work keys and caller tags
```

[README.md](README.md) documents the contracts.

## Building and testing

Tests never run on the host: test processes can leak onto the machine they run
on, so every test run happens inside Dagger. `gofmt`, `go build` and `go vet`
are fine on the host.

For focused iteration, run one package or one test inside a Dagger container
started from the test runtime `dagger check` uses: `test-runtime` of the
`agentcore-dev` module in `.dagger/modules/agentcore-dev`,
`golang:1.26-bookworm` with a pinned `jj` release for `vcs/jj`'s tests. A tool
a test needs is added there.

```sh
dagger call test-runtime \
    with-directory --path /src --source . --exclude .git \
    with-workdir --path /src \
    with-exec --args=go,test,-count=1,-run,'TestA|TestB',-v,./agent \
    combined-output
```

The arguments after `--args=` are the `go test` command line, separated by
commas. Add `-count=5` there to reproduce a flake and `-race` to match the
race detector.

The gate before committing is Dagger:

```sh
dagger check
```

`dagger check -l` lists the checks.

### Testing rules

- Tests never call the real `claude`, `codex`, `opencode`, `pi`, `docker` or
  `sbx`. `agent/agenttest` supplies fake scripts for each, and
  `agenttest.CodexAppServer` makes the test binary itself a fake
  `codex app-server`. Before a session starts, `agent/agentbin.Resolve`
  refuses any other executable when `testing.Testing()` reports a `go test`
  build, whether the binary is running its tests or was started again as a
  command of its own. A test that forgets its fake fails with `ErrRealAgent`
  naming the executable it found on PATH. Engine and host-server launches go
  through the same guard.
- Git is real: tests create repositories under `t.TempDir()`.
- `agent`'s enforcement tests (`TestLandlockHoldsATurnToItsGrants`,
  `TestLandlockEnforcesWhatASessionReports`,
  `TestLandlockLeavesTheRunnerUnconfined`) run a fake agent under the
  kernel's enforcement, and skip, saying why, on a kernel without Landlock.
  Docker Desktop's kernel is one, so `dagger check` on a Mac skips them. This
  runs them on Debian's kernel under QEMU, inside Dagger, with no privileges:

  ```sh
  dagger core container from --address golang:1.26-trixie \
    with-directory --path /src --source . --exclude .git \
    with-exec --args=sh,/src/agent/testdata/landlock-vm/run.sh \
    combined-output
  ```

  Run it after changing `agent/confine*.go` or `enforce.go`. It is not a
  check: the gate stays `dagger check`.
- macOS is a confinement platform too: Seatbelt, through
  `/usr/bin/sandbox-exec`. `dagger check` tests the profile it is given
  (`confine_seatbelt_test.go`), and nothing in it can test macOS's
  enforcement. After changing `agent/confine*.go`, check it by hand on a Mac:

  ```sh
  go run ./agent/testdata/seatbelt-check
  go run ./agent/testdata/seatbelt-check -sandbox claude
  go run ./agent/testdata/seatbelt-check -vcs
  ```

  Each run starts one confined turn whose agent is a shell script. The
  script reads outside its mounts, writes into its read-only working
  directory and runs `/usr/bin/git`. The program prints one line per
  attempt and exits 1 when one was allowed that should have been refused,
  or refused when it should have been allowed. With `-vcs`, `git` is
  allowed.

### Go and markdown style

Match the style of the surrounding code. For markdown, follow the rules in
[AGENTS.md](AGENTS.md#documentation).

## Contributor workflow

1. Branch, make your change, and keep `dagger check` green as you go.
2. Open a pull request. `dagger check` runs on it through Dagger Cloud Checks;
   there is no GitHub Actions workflow.
3. A reviewer goes through the PR. Address feedback and keep `dagger check`
   green until it is approved and merged.

## Releasing

Go reads the module's versions from `v*` tags. Tag a commit on `main` that
passes `dagger check` and push the tag:

```sh
git tag -a v0.6.0 -m "v0.6.0"
git push origin v0.6.0
```

Callers then move to it with `go get github.com/kpenfound/agentcore@v0.6.0`.
