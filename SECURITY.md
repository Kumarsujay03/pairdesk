# Security policy

PairDesk gives one person consent-gated access to another person's machine, so security reports
matter more here than features.

## Reporting

Email **mr.sujaykumar03@gmail.com** with "PairDesk security" in the subject, or open a private
GitHub security advisory on this repository. Please do not file public issues for vulnerabilities.
You will get a reply within 7 days.

## What counts

Anything that lets someone:
- open a terminal, screen or file session without the host's consent or a grant the host signed
- read chat, presence or session logs they are not party to
- escalate past the sudo tier the host chose
- impersonate the admin, a helper or any peer
- tamper with a session log without `/logs verify` noticing
- make the rendezvous server (Supabase) see plaintext or forge a grant

## What is out of scope

- A visitor who already knows the host's sudo password (the *ask* tier) or a `NOPASSWD` sudoers
  rule running `/usr/bin/sudo` directly in a *board* session. The sandbox hides PairDesk's own
  files and credentials and blocks credential caching; it does not jail the visitor from the
  rest of `$HOME`. See PAIRDESK.md §16.
- Denial of service against a free-tier Supabase project.
- Issues in GStreamer, PipeWire, pion or other dependencies (report upstream; tell us too).

## Design

The security model is written up in [PAIRDESK.md](PAIRDESK.md) §7, §8 and §16. Row Level Security
lives in `supabase/migrations/`. Session gating lives in `internal/session/manager.go`.
