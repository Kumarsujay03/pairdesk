# Changelog

All notable changes to PairDesk. The section for a tag becomes that release's notes on GitHub.
Format: [Keep a Changelog](https://keepachangelog.com), versions follow semver.

## [0.1.3] - 2026-11-01

### Added
- In the suggestion strip, `up`/`down` pick a command and `enter` runs it when it needs no
  argument, or fills it in and waits for the argument.

### Fixed
- The suggestion strip pushed the input line out of the chat pane; the input stays visible.

## [0.1.2] - 2026-11-01

### Changed
- GitHub Actions dependencies updated: checkout 7, setup-go 7, codeql-action 4,
  upload-pages-artifact 5, deploy-pages 5.
- Release notes are now taken from this changelog instead of raw commit messages.

## [0.1.1] - 2026-11-01

### Added
- Command suggestions: type `/` for a strip of everything you can run, with descriptions;
  `tab` completes and cycles, including `/admin` and `/group` subcommands. Admin and helper
  commands are only ever suggested to those roles.
- `/update` installs the latest release in place; `/version` shows the running version.
- `/notify on|off` controls desktop pop-ups. Session alerts (consent cards, sudo use, a session
  going live or ending, file offers) now always appear inside PairDesk as well.
- Releases are signed with the PairDesk release key; `README` explains how to verify one.
- `make build` builds with the project baked in from a gitignored `.env`.

### Fixed
- Supabase migrations: pgcrypto is resolved from the `extensions` schema, and base table
  privileges are granted explicitly so a recreated `public` schema works.
- The test suite no longer sends real desktop notifications.
- Two CodeQL findings about size arithmetic in the frame writer and signature prefix.
- Flaky timing in the sudo askpass, sandbox isolation and revoke tests.

## [0.1.0] - 2026-11-01

First release.

### Added
- Terminal messenger for Linux: friends, direct messages, groups, reactions, search, themes.
  End-to-end encryption (X25519 + XChaCha20-Poly1305) with key pinning and fingerprint phrases.
- Consent-gated remote help over WebRTC: the admin or a helper can `/board` or `/watch` a
  friend's terminal, or `/screen` their display, only after the friend accepts a card. Standing
  grants for terminal access (`/grant`, `/revoke`); screen access always asks.
- Visitor shells run in a user/mount/PID namespace sandbox (bubblewrap or unshare) with
  PairDesk's own files hidden. Sudo tiers: blocked, ask, auto, with the host running sudo outside
  the sandbox.
- Hash-chained, signed session logs with command, output and file-change records; `/logs` to
  browse, replay and verify; `/stop` kill switch.
- Admin controls: registration modes and invites, user cap, approval queue, ban/kick/mute,
  helpers, announcements, maintenance mode, minimum version, rate limits, stats, admin log,
  two-factor admin devices and a local admin passphrase.
- Peer-to-peer file drop, shared terminal, desktop notifications, self-updater, one-line installer.
- Supabase (free tier) as rendezvous with Row Level Security and server-side functions.

[0.1.3]: https://github.com/Kumarsujay03/pairdesk/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/Kumarsujay03/pairdesk/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/Kumarsujay03/pairdesk/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/Kumarsujay03/pairdesk/releases/tag/v0.1.0
