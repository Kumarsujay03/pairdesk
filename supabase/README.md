# Backend setup (Supabase)

PairDesk needs one hosted piece: a Supabase project on the free tier. It stores accounts,
friendships, groups, encrypted messages, grants and short-lived signaling rows. It never sees a
plaintext message, a session, or a private key. Setup takes about 15 minutes.

## 1. Create the project

1. Sign in at https://supabase.com and create a new project (free plan, any region near your users).
2. Note two values:
   - the **Project URL**, `https://<project id>.supabase.co` (Project Settings → General)
   - the **publishable key**, `sb_publishable_…` (Project Settings → API Keys). Older projects
     show a legacy **anon** JWT (`eyJ…`) instead; both work.

Both are public by design. Row Level Security is the gate, not these values. Never use the
**secret** key (`sb_secret_…`, formerly `service_role`) anywhere in PairDesk.

## 2. Load the schema

In **SQL Editor → New query**, paste and run each file in this order:

1. `migrations/0001_schema.sql` – tables, helper functions
2. `migrations/0002_policies.sql` – Row Level Security and column grants
3. `migrations/0003_rpc.sql` – server-side functions, realtime policies, privilege grants
4. `migrations/0004_policy_fixes.sql` – policy corrections (group creation, key publishing,
   ownership transfer, replies to the admin)

Already running an instance? Run only the migrations you have not applied yet; each is safe to
run once on top of the previous ones.

Each should finish with "Success, no rows returned". If you need to start over, run
`drop schema public cascade; create schema public;` and rerun from 0001; `0002` restores the table
grants the drop removes.

## 3. Enable Realtime

**Database → Replication** (or **Publications → supabase_realtime**): enable the tables
`messages`, `friendships`, `groups`, `group_members`, `grants`, `instance_settings`, `signals`
and `users`.

Under **Project Settings → Realtime**, make sure private channels / Realtime authorization are
enabled. Presence runs on private per-user channels guarded by the `realtime.messages` policies.

## 4. Email login

**Authentication → Providers → Email**: enabled, with **Email OTP** on (the client asks for the
6-digit code). "Confirm email" may be on or off. The built-in sender allows a few auth emails per
hour, enough for a small crew; add custom SMTP under **Authentication → SMTP** if you outgrow it.

## 5. Build the client with the project baked in

```bash
go build -ldflags "-X main.supabaseURL=https://<ref>.supabase.co -X main.supabaseAnon=<publishable key>" ./cmd/pairdesk
```

For development, environment variables work instead of linker flags:

```bash
export PAIRDESK_SUPABASE_URL=https://<ref>.supabase.co
export PAIRDESK_SUPABASE_ANON=<publishable key>
go run ./cmd/pairdesk
```

## 6. First run and the admin

The **first account to register becomes the admin** and the client mints its admin device token
automatically. Do this before telling anyone else about the project. Then:

- `/admin pass` to set the local admin passphrase
- `/admin reg closed|invite|open` to choose how people join; `/admin invite new --uses 5 --expires 7d`
- `/admin cap <n>` and `/admin approve on` are optional extra gates

Back up `~/.config/pairdesk/identity.key` and `~/.config/pairdesk/login.json` from the admin
machine; together they are the admin's access. Approve further admin devices with `/admin devices`.

## 7. Keep the free project awake

Supabase pauses free projects after a week without traffic. The repository's `keepalive` GitHub
Actions workflow pings the project daily; it needs the repository secrets `SUPABASE_URL` and
`SUPABASE_ANON`. Run it once by hand from **Actions → keepalive** to confirm it prints `awake`.

## 8. Releases

Tagging `v*` runs GoReleaser with the URL and anon key from the same secrets baked in. Add
`GPG_PRIVATE_KEY` and `GPG_FINGERPRINT` to sign `checksums.txt`; without them the release is
published unsigned. `web/install.sh` and `pairdesk --update` install from GitHub Releases and
verify checksums.

## 9. Host requirements for remote sessions

- **bubblewrap** (`bwrap`) is recommended on any machine that will host a board/watch session. The
  visitor's shell runs in a user/mount/PID namespace with PairDesk's own files hidden. Without
  bwrap the client falls back to `unshare`; if neither works the session is refused.
- Screen control needs write access to `/dev/uinput` (`sudo usermod -aG input $USER`, then log
  out and in). Viewing does not.
- Screen capture uses the desktop portal (PipeWire) on Wayland, `ximagesrc` on X11, through
  GStreamer (`gst-launch-1.0` with `vp8enc`, `rtpvp8pay`, `pipewiresrc`).

## 10. Free-tier limits that matter

| Limit | Free tier |
|---|---|
| Database | 500 MB (`/admin stats` shows usage) |
| Realtime concurrent connections | 200 |
| Realtime messages | 2 M per month |
| Monthly active users | 50 k |

Chat and signaling rows are small; the database grows mainly with message history.
