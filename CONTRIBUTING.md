# Contributing

- `go vet ./... && go test -race ./...` must pass. CI runs the same.
- Changes to `supabase/migrations/`, `internal/session/`, `internal/term/`, `internal/crypto/` or
  `internal/p2p/` need a test that exercises the rule you changed, and a line in the PR explaining
  which guarantee from PAIRDESK.md it affects.
- No new dependencies without a reason in the PR. No cgo.
- Keep user-facing text short and plain; the TUI is the product.
- Try the UI with `go run ./cmd/pairdesk --demo` (OTP `000000`).

## Changelog and releases

Every user-visible change gets a line under the next version in `CHANGELOG.md` (Added, Changed,
Fixed, Security). A maintainer cuts a release by dating that section, tagging `vX.Y.Z` and
pushing the tag; the workflow builds, signs and publishes with that section as the notes.
