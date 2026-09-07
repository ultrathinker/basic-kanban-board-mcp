<!-- Keep it focused: one concern per PR. -->

## What and why

<!-- What does this change, and what problem does it solve? -->

## Checklist

- [ ] `go build ./...`, `go vet ./...`, `go test ./...` pass
- [ ] `gofmt -l .` prints nothing
- [ ] Web asset change? Ran `make sync-embed` and committed the mirror
- [ ] Tests added/updated for behavior changes
- [ ] Docs / CHANGELOG updated if the public surface changed
- [ ] No new MCP tool (capability goes into existing tools' parameters)
