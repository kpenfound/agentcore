# Release notes

## Unreleased

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
