# Release notes

## Unreleased

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
