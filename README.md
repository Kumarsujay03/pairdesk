# PairDesk

A terminal messenger for Linux people, with consent-gated remote access.

```
curl -fsSL https://kumarsujay03.github.io/pairdesk/install.sh | sh
pairdesk
```

- **Chat**: pick a username, add friends, DM, make groups. The server stores ciphertext only.
- **Remote help**: the admin (or a helper) can watch or board a friend's terminal, or view their screen, only with that friend's consent.
- **Standing grants**: trust the admin once with `/grant`; revoke any time. `sudo` stays blocked unless you choose otherwise.
- **Logs you own**: every session on your machine is recorded locally: each command, its output, every file touched, hash-chained and signed.
- **Nothing listens**: every connection is outbound. No ports, no router changes.
- **Free**: Supabase free tier for rendezvous, free STUN/TURN, GitHub for everything else.

Full design: [PAIRDESK.md](PAIRDESK.md). Backend setup: [supabase/README.md](supabase/README.md).

## Commands

| | |
|---|---|
| `/add <user>` `/requests` `/remove <user>` | friends |
| `/dm <user>` `/me <text>` `/react 👍` | chat |
| `/group create <name>` `/group invite <user>` `/group leave` | groups |
| `/grant [--watch-only] [--sudo ask\|auto]` `/revoke` `/grants` | trust the admin |
| `/board <user>` `/watch <user>` `/screen <user> [--control]` | admin / helper remote access |
| `/send <user> <path>` `/share` | file drop, shared terminal |
| `/logs` `/stop` | your session records, kill switch |
| `/verify <user>` `/search <text>` `/theme <name>` `/notify on\|off` | misc |
| `/admin …` | instance controls (admin only), see `/help` |

Type `/` and a suggestion strip lists what you can run; `up`/`down` pick, `enter` runs or fills in, `tab` completes and cycles. Admin and helper
commands are only suggested to those roles. `/update` installs the latest release in place.
`ctrl+c` or `/quit` exits. `ctrl+k` ends any live session on your machine.

## Verifying a release

`checksums.txt` on every release is signed with the PairDesk release key, fingerprint
`BE16D22D85EA247C0C9EC497BE61507B01994E87`. The public key is served at
https://kumarsujay03.github.io/pairdesk/pairdesk-release.asc and lives in `web/`.

```
gpg --import pairdesk-release.asc
gpg --verify checksums.txt.sig checksums.txt
sha256sum -c --ignore-missing checksums.txt
```

## Building

```
cp .env.example .env   # fill in your project URL and publishable key once
make build             # -> ~/.local/bin/pairdesk
```

`pairdesk --demo` runs against an in-memory backend with no network (OTP is `000000`), useful for trying the UI.

## Security model, short version

| Layer | Mechanism |
|---|---|
| Transport | TLS 1.3 to Supabase |
| DMs and groups at rest | XChaCha20-Poly1305 with keys derived from device identities (X25519); group keys wrapped per member and rotated on removal |
| Session setup | Noise IK over Ed25519 device identities, signaling encrypted end to end |
| Session data | WebRTC DTLS, certificate fingerprints signed by device keys |
| Grants | Signed by the grantor, enforced by the grantor's client, bound to the grantee's pinned key |
| Visitor shell | User/mount/PID namespace sandbox (bubblewrap or unshare); PairDesk's own files hidden, environment scrubbed, sudo credential caching disabled; session refused if no sandbox |
| Key pinning | Peer and admin keys pinned on first sight; a changed key blocks DMs and sessions until accepted |
| Admin account | Email login **and** an approved device token, plus a local Argon2id passphrase for privileged commands |
| Logs | Per-line hash chain, sealed and signed by both sides |

The server can deny service. It cannot read chat, join a session, impersonate a peer, mint a grant, or make a non-admin account open a session.

## License

MIT
