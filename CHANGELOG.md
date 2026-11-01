# Changelog

All notable changes to PairDesk. The section for a tag becomes that release's notes on GitHub.
Format: [Keep a Changelog](https://keepachangelog.com), versions follow semver.

## [0.1.14] - 2026-11-01

### Fixed
- Live messages now decrypt: Realtime delivers bytea columns as bare hex without the `\x`
  prefix, which the client mistook for base64 (garbage, or a dropped message depending on
  length). Bare hex is now decoded as hex.
- Friends show as online: watcher presence channels opt in with `enabled: true`, which newer
  Realtime servers require before they send presence state to a channel that does not track.

## [0.1.13] - 2026-11-01

### Added
- The terminal window title shows the open conversation (`pairdesk · dm · name`).
- Live records Realtime delivers that the client cannot decode are logged instead of dropped;
  `PAIRDESK_DEBUG_REALTIME=1` logs every raw Realtime frame to `pairdesk.log`.

### Changed
- The bytea repair for Realtime records also handles the variant that includes the `\x` prefix.

## [0.1.12] - 2026-11-01

### Fixed
- Live messages, signals and grants arriving over Realtime failed to decrypt ("cannot decrypt")
  because Supabase Realtime hex-encodes the text form of bytea columns a second time. The client
  now undoes that for change records; history read over REST was never affected.
- Supabase: the presence policy read `users.banned`, a column regular users cannot select, so
  the presence channel was refused and nobody showed as online. Migration
  `0007_presence_policy.sql` fixes it via a security-definer helper.
- Refused Realtime channel joins are now logged in `pairdesk.log`.
- Supabase: `bump_rate` named its parameter after the `bucket` column, so every message, friend
  request and signal failed with `42702 column reference "bucket" is ambiguous`. Migration
  `0006_rate_limit_fix.sql` renames the parameters; the base `0003_rpc.sql` has the same fix.

## [0.1.11] - 2026-11-01

### Added
- Auto-update at start-up: when a newer release exists, the boot screen downloads it with a
  percentage, installs it and restarts into it. `/update auto off` turns this off,
  `PAIRDESK_NO_AUTOUPDATE=1` too.

### Changed
- `/update` shows live progress (checking, download percentage, checksum) on one line in the
  chat and restarts PairDesk by itself once the new release is installed. `--update` on the
  command line shows the same moving percentage.

## [0.1.10] - 2026-11-01

### Fixed
- The installer and `--update` find the latest release through the github.com redirect instead
  of the GitHub API, so shared networks that exhaust the API's 60 requests/hour no longer get
  `403 … could not find the latest release`. The API remains a fallback.

## [0.1.9] - 2026-11-01

### Changed
- With a start-up passkey set, entering it at launch unlocks the admin passphrase for the whole
  run instead of every 15 minutes. `/admin lock` still relocks until the next `/admin unlock`.
  Without a passkey the 15 minute window is unchanged.

## [0.1.8] - 2026-11-01

### Changed
- The start-up passkey may be as short as 4 characters (the admin passphrase stays at 8).

## [0.1.7] - 2026-11-01

### Added
- Optional start-up passkey: `/passkey set` makes PairDesk ask for it every time it starts on
  that machine (Argon2id, stored locally); `/passkey off` removes it. Any user can set one.
- `/logout` signs out on this device. The identity key and, for the admin, the approved
  device token are kept, so signing back in on the same machine needs no new device approval.
- Daily digest: on the first open of each day the chat lists unread conversations, friend
  requests and, for the admin, sign-ups waiting plus the registration and approval modes.
- The admin's status bar shows the registration mode (`reg open|closed|invite`).

## [0.1.6] - 2026-11-01

### Added
- Approval queue for the admin: a notice (and desktop pop-up) the moment someone registers
  while approval is required, `/admin pending` to list who is waiting, a `pending` tag in the
  everyone list, and a count of waiting sign-ups at start-up.

### Fixed
- `supabase/migrations/0005_anon_settings.sql` (run once): the client could not read instance
  settings before signing in, so "registration closed" and maintenance notices only appeared
  after login.

## [0.1.5] - 2026-11-01

### Added
- A real start-up screen: the banner reveals itself while the client signs in, with a spinner
  and the current step (reaching the rendezvous, signing in, loading friends, connecting live).
  A failed start shows the reason with `r` to retry.
- Failed server requests are recorded in `~/.local/state/pairdesk/pairdesk.log` with method,
  path, status and the server's code and message, never tokens or bodies.
- `up`/`down` scroll the chat three lines at a time when the suggestion strip is closed, so long
  output such as `/help` can be read; `pgup`/`pgdn`, `home` and `end` still work.

### Fixed
- Database policies, in the new `supabase/migrations/0004_policy_fixes.sql` (run it once on an
  existing project): creating a group, publishing a device key on a new device or after
  `/admin reset-key`, transferring group ownership, and answering a session card from an admin
  who is not your friend were all refused with `403`.
- Writes that do not need the row back now ask the server for `return=minimal`, so row-level
  read policies can no longer reject a successful insert.
- A second, not-yet-approved admin device now gets a clear message instead of `403` on every
  admin command; a live change to your own account is re-read in full, so bans and kicks
  arrive immediately.
- Admin commands failed with `403 forbidden` when the server no longer recognised the device's
  admin token, for example after the database was recreated. The client now verifies the token
  at start-up and bootstraps a fresh one when it is stale.

## [0.1.4] - 2026-11-01

### Added
- The status bar shows the running version. The client checks GitHub for a newer release on
  start and every six hours; when one exists the bar says `vX.Y.Z available · /update` and a
  note appears in the chat. Dev builds and demo mode never check.

### Changed
- On narrow terminals the status bar drops the theme name, then the key hints, instead of
  wrapping onto a second line.

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

[0.1.8]: https://github.com/Kumarsujay03/pairdesk/compare/v0.1.7...v0.1.8
[0.1.7]: https://github.com/Kumarsujay03/pairdesk/compare/v0.1.6...v0.1.7
[0.1.6]: https://github.com/Kumarsujay03/pairdesk/compare/v0.1.5...v0.1.6
[0.1.5]: https://github.com/Kumarsujay03/pairdesk/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/Kumarsujay03/pairdesk/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/Kumarsujay03/pairdesk/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/Kumarsujay03/pairdesk/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/Kumarsujay03/pairdesk/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/Kumarsujay03/pairdesk/releases/tag/v0.1.0
