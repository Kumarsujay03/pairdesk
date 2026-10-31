# Contributing

- `go vet ./... && go test -race ./...` must pass. CI runs the same.
- Changes to `supabase/migrations/`, `internal/session/`, `internal/term/`, `internal/crypto/` or
  `internal/p2p/` need a test that exercises the rule you changed, and a line in the PR explaining
  which guarantee from PAIRDESK.md it affects.
- No new dependencies without a reason in the PR. No cgo.
- Keep user-facing text short and plain; the TUI is the product.
- Try the UI with `go run ./cmd/pairdesk --demo` (OTP `000000`).
