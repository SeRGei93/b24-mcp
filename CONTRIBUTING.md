# Contributing

1. Use Go 1.24+.
2. Copy `.env.example` to `.env` and fill in local credentials.
3. Verify changes with `go vet ./...`, `go test ./...`, and `CGO_ENABLED=0 go build ./cmd/b24`.
