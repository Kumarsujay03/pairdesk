# PairDesk

**A terminal-native messenger for Linux people. Pick a username, add friends, DM, make groups.
The operator can, with your consent, pair with your desk: your terminal, or your screen.**

PairDesk is public and open source. Anyone can install it and chat. Remote access is a separate,
locked-down layer: only the **admin** (the person running the PairDesk instance) can ask for a
terminal or screen session, only from a **friend**, and only with that friend's consent.

One static Go binary, animated TUI, nothing listening on any port, everything encrypted end to end,
hosted entirely on free tiers.

---

## 1. Roles and what each can do

| | User | Admin |
|---|---|---|
| Register a unique username | ✔ | ✔ |
| Send / accept friend requests | ✔ | ✔ |
| DM friends | ✔ | ✔ |
| DM **anyone**, friend or not | ✘ | ✔ |
| Show or hide the admin badge | — | ✔ |
| Create groups, invite friends | ✔ | ✔ |
| See who is online | friends only | **everyone** |
| See the full user list | ✘ | ✔ |
| Ban / remove a user | ✘ | ✔ |
| Ask for terminal access (`/board`, `/watch`) | ✘ | ✔, friends only |
| Ask for screen access (`/screen`) | ✘ | ✔, friends only |
| Grant the admin standing terminal access | ✔ | — |
| Approve or deny any access request | ✔ (always the host's call) | — |
| Read the full log of every session on their machine | ✔ | ✔ (own copy) |
| Helper role: request access from *their own* friends | only if promoted | grants / revokes it |
| Open / close registration, invite codes, user cap | ✘ | ✔ |
| Broadcast announcements, maintenance mode | ✘ | ✔ |
| Kick sessions, mute, force re-login, dissolve groups | ✘ | ✔ |
| Instance stats and admin audit log | ✘ | ✔ |

There is exactly one admin per instance. The role lives in the database and is set once by the
operator; no command can elevate a user. The admin may promote **helpers** (section 2.5), who get
remote-access and file-drop rights over their own friends and nothing else.

---

## 2. Admin controls

The admin runs the instance and holds every lever. All of these are `/admin …` commands inside the
TUI, backed by an `instance_settings` table only the admin can write and an `admin_log` every
action lands in.

### 2.1 Onboarding
| Command | Effect |
|---|---|
| `/admin reg open` | Anyone can register a username |
| `/admin reg closed` | Registration off. New installs see "registration is closed" after the banner and can only retry later |
| `/admin reg invite` | Registration requires an invite code |
| `/admin invite new [--uses 5] [--expires 7d]` | Mint an invite code; share it however you like |
| `/admin invite list / revoke <code>` | Manage codes |
| `/admin cap 50` | Hard ceiling on total users; registration auto-closes at the cap |
| `/admin approve` | Optional second gate: new registrations land in a queue the admin approves or denies, even when open |

Registration is enforced server side in the `register_username` RPC (checks mode, cap, code, ban
list), not just hidden in the client.

### 2.2 People
| Command | Effect |
|---|---|
| `/users` | Everyone, with ● / ○, last seen, device fingerprint, friend count, groups |
| `/admin inspect <username>` | Full card: join date, invite used, grants, session history with you |
| `/ban <username> [reason]` / `/unban` | Ban: every RLS policy rejects them, their sessions end, friends see them as removed |
| `/admin kick <username>` | Force sign-out everywhere; they must log in again |
| `/admin mute <username> [1h]` | Can read, cannot send |
| `/admin reset-key <username>` | Invalidate their device key; next launch forges a new one and all their friends get a key-change warning (for a lost or stolen laptop) |
| `/admin rename <old> <new>` | Resolve a username dispute |

### 2.3 Groups
| Command | Effect |
|---|---|
| `/admin groups` | All groups, owners, member counts (names only, never content) |
| `/admin group dissolve <name>` | Delete a group; members are notified |
| `/admin group transfer <name> <username>` | Reassign ownership |

The admin cannot read group or DM content. Keys are never wrapped for the admin unless they are a
member or party. This is a deliberate line: operator, not reader.

### 2.4 Instance
| Command | Effect |
|---|---|
| `/admin announce <text>` | Pinned banner at the top of every user's TUI until dismissed |
| `/admin maintenance on [msg]` | All non-admin clients drop to a read-only "back soon" screen |
| `/admin min-version 1.4.0` | Older clients are told to `--update` and refused |
| `/admin limits msgs 30/min` | Per-user rate limits for messages, friend requests, group creates |
| `/admin stats` | Users total / online / today, messages today, sessions this week, DB size vs free-tier limit, Realtime connections vs 200 cap |
| `/admin log [--tail 50]` | Admin audit log: every admin action, timestamped |
| `/admin export` | Dump users, friendships, groups, settings as JSON for backup (no message content, it is ciphertext anyway) |

### 2.5 Remote access (admin only)
| Command | Effect |
|---|---|
| `/board`, `/watch`, `/screen` | As in section 4; only on friends, only with consent or grant |
| `/admin sessions` | Live sessions with duration and a kill key |
| `/admin grants` | Every standing grant users have given you |
| `/admin revoke-all` | Panic button: end every live session and drop every grant on your side |
| `/admin helper add <username>` | Promote a trusted user to **helper**: they may `/board`, `/watch`, `/screen` and `/send` to *their own friends* only, with the same consent rules. They get no other admin power. Shown as `[helper]` on access cards |
| `/admin helper remove <username>` | Demote; their live sessions end and users' grants to them are suspended |

### 2.6 Reach and visibility

**The admin can message anyone.** `/dm <username>` works for the admin on every registered user,
friend or not. The conversation opens on the user's side as a normal DM with a one-line note the
first time: `sujay03 started this chat`. The user can reply in it, block nothing (the admin is the
operator), but is never forced to be friends. Encryption is the same pairwise key as any DM, derived
from both device identities, so friendship is a social gate, not a cryptographic one. RLS allows a
DM row whenever the two parties are friends **or** one of them is the admin.

**Badge rules.** Who is admin is visible or hidden depending on where the name appears:

| Where | Shown as | Why |
|---|---|---|
| Announcements, maintenance screen, system broadcasts | **`admin`** (never the username) | These speak for the instance, not the person |
| DMs, groups, friends pane, everyone pane | username only, e.g. `sujay03` | Default: the admin looks like any other user |
| Same, after `/admin badge on` | `sujay03 [admin]` | Opt-in, per the admin's mood |
| Access request cards (`board`, `watch`, `screen`) | `sujay03 [admin]` **always** | The user must know an operator is asking; this one cannot be hidden |

| Command | Effect |
|---|---|
| `/admin badge on` | Show `[admin]` next to the admin's username everywhere personal |
| `/admin badge off` | Hide it (default) |

Hiding the badge is cosmetic. It never changes permissions, RLS, or the signed-admin-key check
clients run on access requests, and `/admin inspect` style commands still work while hidden.

### 2.7 Securing the admin account
The admin is the most valuable account on the instance, so it gets extra locks:

- **Two factors to be admin.** Magic-link login *and* a registered admin device key. A fresh
  device logging in with the admin email is a plain user until it is approved from an existing
  admin device (`/admin devices approve`). The role check in RLS is `role = admin AND device_id IN
  admin_devices`.
- **Admin passphrase.** `/admin …` commands and every remote-access command ask for a local
  passphrase (Argon2id, stored only on the admin's machine) once per 15 minutes. A stolen unlocked
  terminal cannot board anyone.
- **Pinned admin key on every client.** Clients only treat an incoming session request as "from the
  admin" if it is signed by the admin key they have pinned. A rogue database row claiming
  `role = admin` for another account is ignored.
- **Hardware-backed option.** If a FIDO2 key is present, the admin device key can live on it
  (future add-on; v1 uses an encrypted file).
- **Immutable admin log.** `admin_log` is insert-only for everyone, including the admin.
- **Recovery.** A one-time recovery code is printed when the admin role is first set. Losing all
  admin devices and the code means a new instance, by design.

---

## 3. First run

```
curl -fsSL https://pairdesk.is-a.dev/install.sh | sh
pairdesk
```

1. Animated banner.
2. **Registration check.** If the admin has closed registration you see a friendly wall and stop
   here. In invite mode you are asked for a code first.
3. **Choose a username.** 3 to 20 chars, `[a-z0-9_]`, checked live against the database as you
   type: `sujay ✘ taken` → `sujay03 ✔ available`. This is the name friends will use to find you.
4. Magic-link login (email) binds the username to an account.
5. Device keypair is forged (short animation, fingerprint phrase revealed word by word).
6. If the admin has turned on approval mode, a "waiting for approval" spinner; otherwise you land
   in the TUI with an empty friends list and a hint: `/add <username>`.

Whether joining needs approval, an invite, or nothing at all is the admin's switch (section 2.1).

---

## 4. What it feels like

### User view

```
┌ friends ──────────┐┌ dm · arjun ────────────────────────────────────────┐
│ ● arjun           ││ 14:02 arjun   anyone free to look at my nginx conf  │
│ ● sujay03         ││ 14:03 you     ping sujay, he can board you          │
│ ○ priya           ││                                                      │
│ ○ dev_rahul       ││ arjun is typing…                                     │
│                   ││ > _                                                  │
│ groups            ││                                                      │
│ # linux-lab (2●)  ││                                                      │
│ # college   (0●)  ││                                                      │
└───────────────────┘└──────────────────────────────────────────────────────┘
 ● online  ○ offline  ·  tab panes  ·  ctrl+c quit  ·  /help
```

### Admin view

Same layout, plus an **everyone** pane the admin can toggle with `/users`:

```
┌ everyone (14) ────────┐
│ ● arjun               │
│ ● priya               │
│ ● dev_rahul           │
│ ○ kiran               │
│ ○ meera        banned │
│ …                     │
└───────────────────────┘
```

Filled circle = online right now. Hollow circle = offline. The dot pulses once when someone comes
online. Group names show how many members are online in brackets.

### Slash commands

| Command | Who | What |
|---|---|---|
| `/add <username>` | all | Send a friend request |
| `/requests` | all | List pending requests; `a`/`d` to accept or deny inline |
| `/remove <username>` | all | Unfriend |
| `/dm <username>` | all | Open or switch to a DM (admin: works on anyone, friend or not) |
| `/group create <name>` | all | Create a group (you are its owner) |
| `/group invite <username>` | group owner | Invite a friend |
| `/group leave` | all | Leave the current group |
| `/friends` | all | Friends pane with online dots |
| `/users` | admin | Everyone pane with online dots |
| `/admin …` | admin | Instance controls, see section 2 |
| `/ban <username>` / `/unban` | admin | Ban or restore a user |
| `/board <username>` | admin | Interactive shell on a friend's machine |
| `/watch <username>` | admin | Read-only view of a friend's terminal |
| `/screen <username>` | admin | View a friend's screen in a popup window; `--control` adds input |
| `/grant` | user | Standing terminal access for the admin while you are online. **sudo blocked** |
| `/grant --watch-only` | user | Standing grant for `/watch` only |
| `/grant --sudo ask` | user | Same, and the admin may run `sudo` if they know your password (you tell them) |
| `/grant --sudo auto` | user | Same, and `sudo` works without you present: password kept in your keyring, never shown to the admin |
| `/stop` | user | Kill any live session on your machine instantly (same as `ctrl+k`) |
| `/revoke` | user | Withdraw the standing grant |
| `/end` | both | End the current session |
| `/logs` | all | Browse every session anyone has had on *your* machine: commands, output, files touched |
| `/logs replay <id>` | all | Replay a terminal session like a recording |
| `/send <username> <path>` | all | Drop a file to a friend, peer to peer |
| `/share` | admin / helper, in a session | Turn a board session into a shared terminal (both type) |
| `/search <text>` | all | Search your local, decrypted chat history |
| `/theme <name>` | all | Switch theme |
| `/admin helper add <username>` / `remove` | admin | Promote or demote a helper |
| `/verify <username>` | all | Show a 4-word fingerprint phrase to compare out of band |
| `/quit` | all | Exit (also `ctrl+c`) |

Non-admins who type `/board`, `/watch` or `/screen` get a one-line explanation, nothing more.
The server also enforces it: only the admin's account can open a signaling channel.

---

## 5. Flows

### 4.1 Friend request

```
  priya                      Supabase                       arjun
    │  /add arjun               │                             │
    ├──── insert friendship ───►│  status=pending             │
    │                           ├──── realtime event ────────►│
    │                           │              request card slides into arjun's TUI
    │                           │◄──── update status=accepted ┤   arjun presses [a]
    │◄──── realtime event ──────┤                             │
    │  "arjun accepted"         │  both clients derive the pairwise DM key
    │                           │  both subscribe to each other's presence channel
```

Friendship is symmetric. Denying is silent to the requester (it just stays "pending" on their side
and expires after 7 days). A denied requester cannot re-request for 24 hours.

### 4.2 Direct message

```
  priya ──── encrypt(pairwise key) ────► messages table ────► arjun decrypts
```

The server stores ciphertext only. The pairwise key comes from X25519 on both device identities
(Noise-style), so no key ever travels through Supabase.

### 4.3 Group

```
  owner creates group ──► group_key generated locally
  owner invites friend ──► group_key wrapped with friend's X25519 pubkey ──► group_members row
  anyone leaves/removed ──► owner rotates group_key, rewraps for remaining members
```

Groups are invite-only and you can only invite your own friends. Members of the same group who are
not friends with each other can read the group but cannot DM each other until they `/add`.

### 4.4 Admin remote access

```
  admin                       Supabase                    friend (host)
    │  /screen arjun             │                             │
    │  (admin? friend?) ─── checked locally AND by RLS ───►    │
    ├──── encrypted offer ──────►│ broadcast:session:<id>      │
    │                            ├────────────────────────────►│
    │                            │          consent card: VIEW YOUR SCREEN [a]/[d] 60s
    │                            │          (terminal cards also offer [s] = approve with sudo)
    │                            │◄──── encrypted answer ──────┤  arjun presses [a]
    │◄───────────────────────────┤                             │
    │◄════════ WebRTC DTLS, direct UDP (STUN / TURN) ═════════►│
    │  viewer window opens       │        red bar: "sujay03 is viewing · ctrl+k"
```

Terminal access is identical except that, if the host has run `/grant`, the consent card is skipped
and the session opens immediately. The host bar still appears. Screen never skips the card.

### 4.5 Standing grant

```
  arjun types /grant ──► confirmation card explains scope ──► [y]
      └─► grant row {grantor: arjun, grantee: admin, scope: board, sig} ──► Supabase (hint)
      └─► signed copy in ~/.config/pairdesk/grants.json (enforced)
  later: admin /board arjun ──► arjun's client checks local grant ──► auto-accept ──► bar shows
  arjun types /revoke ──► row revoked, local copy deleted, any live session ends
```

A grant is bound to the admin's pinned key. If the admin's device key changes, the grant suspends
until the user re-verifies. Its sudo tier (section 4.7) is signed into the same row.

### 4.5b Session lifetime and the host's kill switch

```
  request/grant ──► ACCEPTED ──► logging starts ──► session live ──► /end or /stop ──► logging sealed
                      ▲ the host can /stop at any point from here on, including mid-command
```

Logging begins the instant a session is accepted (or auto-accepted under a grant), before the first
byte reaches the visitor, and is sealed and signed when the session ends for any reason. `/stop`
or `ctrl+k` on the host ends the session at once: PTY closed, child processes killed, viewer window
on the admin's side closed, chain signed. The admin's pane folds shut with `ended by host`.

### 4.6 File drop (peer to peer, torrent style)

```
  priya /send arjun big.iso
     └─► manifest {name, size, sha256, chunk_size, chunk_hashes[]} ──encrypted DM──► arjun
  arjun accepts card ──► WebRTC data channel opens (same handshake as a session)
     └─► chunks requested by index, verified against chunk hash, written sparse, resumable
  group: /send #linux-lab big.iso ──► every member who finished becomes a seed;
         later joiners pull chunks from whichever seeds are online (rarest-first)
```

No Supabase Storage, no size limit beyond disk. Transfers survive reconnects because every chunk is
independently hashed. Files land in `~/Downloads/pairdesk/` after the recipient accepts a card
showing name, size and sender. Helpers and the admin can `/send` to anyone they can session with;
users to friends.

### 4.7 Sudo policy

`sudo` is the line between "help me with my files" and "own my machine", so it is handled apart
from everything else. Inside every visitor shell, PairDesk puts a `sudo` shim first in `PATH`. The
shim decides what happens based on the **tier** the host chose:

| Tier | How the host sets it | What happens when the visitor runs `sudo …` |
|---|---|---|
| **blocked** (default) | `/grant`, or `[a]` on a card | The shim does not run the command. A card appears on the host: `sujay03 wants to run: sudo apt install nginx` with `[a]/[d]`. On `[a]` the **host** types their password locally; the visitor sees only the output. Nobody present → denied after 60 s |
| **ask** | `/grant --sudo ask`, or `[s]` on a card | The real `sudo` runs and prompts in the visitor's pane. The visitor must type the host's password, which the host chose to share with them. The password keystrokes never enter the transcript or log |
| **auto** | `/grant --sudo auto` | The host enters their password once, locally, into the system keyring (libsecret). The shim feeds it to `sudo -S` for each command. The visitor never sees it. The host is notified of every sudo command and can `/revoke` at any time |

Rules that hold in every tier:
- The tier is part of the signed grant or the signed card answer, so the server cannot upgrade it.
- A card-based session is **blocked** unless the host explicitly picks `[s]`.
- `auto` is the only tier that works while the host is away. `/grant --sudo auto` shows a plain
  warning before it is accepted: *the admin can do anything root can do on this machine while you
  are online.*
- Every sudo command, its tier, exit code and output is logged like any other command, flagged
  `sudo: true`. Password input is redacted at the PTY layer, so even the transcript never holds it.
- The admin's own side needs the admin passphrase (section 2.7) unlocked to send a sudo command,
  on top of the host's tier. Both locks must be open.
- `/stop` or `/revoke` on the host kills the shell and any running sudo child immediately.

### 4.8 Shared terminal

Inside a live `board` session the visitor types `/share`; the host gets a card. On accept the PTY
becomes two-way for both: both see the same shell, both can type, the host bar reads
`shared with sujay03`. Every command in the log is tagged with who typed it. Either side `/end`s.

---

## 6. Architecture

```
              ┌────────────────────────────────────────┐
              │  Supabase (free tier)                   │
              │  Auth · users · friendships · groups    │
              │  encrypted messages · grants · presence │
              │  signaling broadcast                    │
              └─────┬──────────────┬──────────────┬─────┘
          HTTPS/WSS │              │              │
         ┌──────────▼──┐   ┌───────▼─────┐   ┌────▼────────┐
         │ pairdesk    │   │ pairdesk    │   │ pairdesk    │
         │ priya       │   │ arjun       │   │ sujay03 ★   │
         └─────────────┘   └──────┬──────┘   └──────┬──────┘
                                  │                 │
                                  └═══════◄►════════┘   WebRTC, only ever
                                   direct UDP            admin ◄► friend
```

Chat traffic (DM, group) goes through Supabase as ciphertext. Remote sessions go peer to peer and
only ever between the admin and one friend.

### 5.1 Client (Go, single static binary)

| Concern | Choice |
|---|---|
| TUI | Bubble Tea + Lip Gloss + Bubbles |
| Animation | Harmonica springs for panes and cards; tick timers for spinners, typing dots, countdowns |
| Peer to peer | pion/webrtc |
| PTY | creack/pty |
| Screen capture | PipeWire via xdg-desktop-portal (Wayland and X11), VP8 |
| Screen viewer window | go-sdl2, spawned as a child process |
| Input injection | `uinput` (host user in the `input` group; install.sh offers to add them) |
| Crypto | Ed25519 identity, X25519 + XChaCha20-Poly1305, Noise IK handshake, DTLS from pion |
| Config | `~/.config/pairdesk/` (`identity.key` 0600, `grants.json` signed, `pins.json`) |

### 5.2 Database (Supabase free tier)

| Table | Columns | Notes |
|---|---|---|
| `users` | `id, username (unique, citext), pubkey, role (user/admin), banned, created_at` | username checked by a `unique` index + an RPC for live availability |
| `friendships` | `requester, addressee, status (pending/accepted/denied), created_at` | one row per pair, enforced by a check on `(least, greatest)` |
| `groups` | `id, name, owner` | |
| `group_members` | `group_id, user_id, wrapped_key` | key wrapped for that member |
| `messages` | `id, conv_type (dm/group), conv_id, sender, ciphertext, nonce, created_at` | ciphertext only |
| `grants` | `grantor, grantee, scope (board/watch), created_at, revoked_at, signature` | hint only; enforced client side |
| `sessions_log` | `id, admin, host, kind, started_at, ended_at, via (grant/card)` | admin-side audit, no content |
| `instance_settings` | `key, value` | registration mode, cap, min version, limits, maintenance, announcement; admin writes, everyone reads |
| `invites` | `code, uses_left, expires_at, created_by, used_by[]` | checked inside `register_username` |
| `admin_devices` | `device_id, pubkey, approved_at` | which devices may act as admin |
| `admin_log` | `id, action, target, at` | insert-only, including for the admin |
| `rate_limits` | `user_id, bucket, count, window_start` | enforced in RPCs |

Realtime:
- `presence:user:<id>` – each client tracks itself here. **Private channel.** Join policy: you are
  that user, or an accepted friend of that user, or the admin.
- `conv:<id>` – broadcast of new-message notifications for DMs and groups (payload is just an id;
  clients fetch the ciphertext row).
- `session:<id>` – WebRTC signaling. Join policy: admin + the one friend named in the session.

Row Level Security (the important ones):
- `users`: everyone reads `username, pubkey, role` of anyone (needed for `/add` and key lookup);
  only the admin reads `banned` and the full list with emails.
- `friendships`: insert as requester only; update `status` as addressee only.
- `messages`: insert/select only if you are a party to the conversation (friend for DM, member for
  group). DM exception: a row is also allowed when either party is the admin, so the admin can open
  a chat with anyone.
- `grants`: insert/revoke as grantor only; `grantee` must be the admin.
- `session:*` channel: only `role in (admin, helper)` can create, and a helper only with an accepted
  friend; only the named friend can join.
- Banned users: every policy includes `and not banned`.
- `instance_settings`, `invites`, `admin_devices`: write only when `role = admin` **and** the
  caller's device is in `admin_devices`.
- `register_username` RPC: refuses when mode is closed, cap reached, invite invalid, or email banned.
- Maintenance mode: a single policy predicate `not maintenance() or is_admin()` on every write.

Free-tier limits that matter: 500 MB database, 200 concurrent Realtime connections, 2 M Realtime
messages per month, 50 k monthly active users on Auth. For a public hobby project that is a long
way off. A daily GitHub Actions ping keeps the project from pausing.

### 5.3 Peer-to-peer sessions

- Signaling via the `session:<id>` broadcast channel; payloads encrypted with the pairwise key.
- STUN: Google or Cloudflare, free. TURN fallback: Cloudflare Realtime (1 TB/month free) or
  metered.ca (20 GB free). TURN relays ciphertext only.
- Terminal: PTY bytes on a reliable ordered data channel, rendered as a pane in the admin's TUI.
- Screen: PipeWire → VP8 → video track → SDL viewer window on the admin's machine.
- Control: key and pointer events on a data channel, `uinput` on the host, 30-minute expiry.

---

## 7. Session logs: what the admin did on your machine

Every remote session is recorded **on the host's machine**, by the host's own client, in a form the
host can read any time and nobody can quietly edit. The admin (or helper) keeps an identical copy on
their side. Both are written by the same code from the same stream, so they match.

### 7.1 What gets logged

| Session kind | Recorded |
|---|---|
| `watch` | Start / end time, who, via grant or card, full terminal transcript (asciicast v2, timed) |
| `board` | Everything above **plus** each command line the visitor ran, its exit code, and its full output, as separate entries, **plus** every file opened, created, modified or deleted while the session was live |
| `screen` | Start / end, who, view or control, number of input events; optional thumbnails every 30 s if the host enables `log.screen_thumbs` |
| `share` | As `board`, with each command tagged by who typed it |
| `send` | File name, size, hash, direction, who, time |
| any `sudo` | Flagged `sudo: true` with the tier in force; password input redacted at the PTY layer |

### 7.2 How it is captured

- **Transcript.** The PTY stream is teed into an asciicast file as it is sent to the visitor.
  `/logs replay <id>` plays it back in the TUI with the original timing.
- **Commands and output.** The host spawns the visitor's shell with a logging hook injected
  (`PROMPT_COMMAND` + `DEBUG` trap for bash, `fish_preexec` / `fish_postexec` for fish, `preexec`
  for zsh). Each hook writes a structured entry: `{ts, cmd, cwd, exit, out_offset, out_len}`, where
  the offsets point into the transcript so the exact output of each command can be shown on its own.
  If the visitor starts a sub-shell or a TUI program, the transcript still has everything; only the
  per-command split degrades.
- **Files.** For the lifetime of the session the host client runs an `inotify` (fanotify when
  available) watcher over `$HOME` and any path the visitor `cd`s into, and records
  `{ts, path, event: open|create|modify|delete|rename, by_cmd}`. Events are correlated with the
  command running at that moment. Paths under `~/.config/pairdesk` and the log directory itself are
  excluded from access by the visitor's shell (mounted read-only via `unshare` where the kernel
  allows; otherwise the client refuses to proceed and tells the host).
- **Modified-file diffs.** For text files under 1 MB that changed during the session, the watcher
  stores a before/after diff so the host can see exactly what was edited.

### 7.3 Tamper evidence

- Each session is a directory `~/.local/state/pairdesk/sessions/<id>/` with `meta.json`,
  `transcript.cast`, `commands.jsonl`, `files.jsonl`, `diffs/`.
- Every line appended to `commands.jsonl` and `files.jsonl` carries a SHA-256 of the previous line
  (hash chain). At session end the final hash is signed with the **host's** device key and written
  to `meta.json`. Editing any line breaks the chain; `/logs verify <id>` checks it.
- The visitor's copy carries the same chain signed with **their** key. Comparing the two final
  hashes proves neither side altered the record.
- Log files are `0600`, owned by the host user, and the visitor's shell cannot reach them (above).
- Retention is the host's choice: `/logs keep 90d` or `forever`. Deleting is a host-only action
  and is itself logged in a small `deletions.log`.

### 7.4 Reading it

`/logs` opens a browser pane:

```
┌ sessions on this machine ───────────────────────────────────────┐
│ 2026-11-01 14:03  sujay03 [admin]  board   18m   grant   ✔ chain │
│   › 23 commands · 7 files modified · 2 files created             │
│ 2026-10-29 21:40  sujay03 [admin]  screen  6m    card    ✔ chain │
│ 2026-10-27 10:12  dev_rahul [helper] watch 3m    card    ✔ chain │
└──────────────────────────────────────────────────────────────────┘
 enter: open · r: replay · f: files · v: verify · d: delete
```

Opening a `board` session shows the command list; each command expands to its output; `f` lists
touched files with diffs. The same viewer is available outside the TUI as `pairdesk logs`.

### 7.5 What it does not do

- It does not record anything while no session is active.
- It does not watch paths outside `$HOME` unless the visitor enters them, and never `/proc`, `/sys`
  or `/dev`.
- It does not send logs anywhere. The server never sees them; `/admin stats` only counts sessions.

---

## 8. Encryption model

| Layer | Protects | Mechanism |
|---|---|---|
| Transport | Everything in flight to Supabase | TLS 1.3 |
| DMs at rest | Content from the server | Pairwise X25519 key from both device identities, XChaCha20-Poly1305 |
| Groups at rest | Content from the server and ex-members | Group key, wrapped per member, rotated on leave/remove |
| Session setup | Signaling, mutual auth | Noise IK on Ed25519 identities |
| Session data | Terminal bytes, screen frames, input | WebRTC DTLS-SRTP, fingerprint signed by device key |

Trust:
- Device key generated on first run; public half stored on the `users` row.
- `/verify <username>` shows a 4-word phrase; comparing it pins the key. Key change after pinning
  is a loud warning and suspends any grant.
- The server can deny service. It cannot read a DM or group, cannot join a session, cannot
  impersonate anyone, cannot mint a grant, and cannot make a non-admin account open a session
  channel (RLS) or be accepted as one by a client (local role check against the pinned admin key).

Hygiene:
- Screen: one consent per session, no exceptions.
- Terminal under grant: silent start, breathing host bar, one-key kick.
- Both sides write `~/.local/state/pairdesk/audit.log`.

---

## 9. Animation and interaction

Reduced-motion switch in config for slow SSH links.

**Launch** · typewriter banner → username field with live ✔/✘ availability → key forging bar with
fingerprint phrase word by word → friends pane springs in.

**Chat** · messages spring up into place; own messages dim until acked; typing dots; presence dot
pulses on online, dims on offline; unread divider that fades; reactions pop inline; `/me` in italics;
long messages collapse with an expand hint; inline code tinted.

**Social** · friend request and group invite cards slide in; accept flashes green, deny shakes;
new friend's dot "lights up" the first time they come online.

**Remote** · consent card with shrinking countdown bar; handshake animation (two dots travel and
lock on DTLS); session pane springs open with an accent border; host bar breathes; kick folds the
pane shut.

**Admin** · `/users` pane slides over the friends pane; ban animates the row grey; `/requests`
is a modal list.

---

## 10. Phases

### Phase 1 · Usernames, friends, DMs
- Install, binary, launcher, banner
- Username registration with live availability, magic-link login, device key
- Friend requests, accept/deny cards, friends pane with ● / ○
- Encrypted DMs, typing indicator, reactions
- Private presence channels with friend-only join policy
- Admin role in DB, admin device registration, `/users` pane with everyone's ● / ○, `/ban`
- Admin DM to anyone, badge on/off, `admin` as sender on announcements
- Registration switch: open / closed / invite / approval, invite codes, user cap
- `/admin announce`, `/admin stats`, admin audit log
- Keep-alive cron

**Done when:** strangers can install, pick names, find each other, DM privately, and the admin can see the whole list with live dots.

### Phase 2 · Groups and admin hardening
- `/group create / invite / leave`, group key wrapping and rotation
- Group pane with online count, owner controls, admin dissolve / transfer
- Admin passphrase, maintenance mode, min version, rate limits, kick, mute, reset-key

**Done when:** friends can run a private encrypted group chat.

### Phase 3 · Terminal watch (admin, per-request)
- Noise IK handshake, pion data channel, STUN
- Admin-only command gate (client + RLS)
- Consent card with countdown on the host, PTY streaming into an admin pane
- Handshake animation, host bar
- Session log v1: transcript, hash chain, signed `meta.json`, `/logs` browser and replay

### Phase 4 · Terminal board and standing grants
- Interactive PTY, resize, TURN fallback
- `/grant`, `/revoke`, signed rows, local enforcement, pin binding
- `/verify`
- Session log v2: per-command entries with output, file watcher, diffs, log dir shielded from the visitor's shell
- Sudo shim with blocked / ask / auto tiers, `[s]` on cards, keyring storage for auto, password redaction
- `/stop` kill switch, logging from acceptance to seal

### Phase 5 · Screen view and control
- PipeWire capture, VP8, SDL viewer, always per-request
- `--control` with `uinput`, clipboard opt-in, 30-minute expiry
- Screen session logging (times, control events, optional thumbnails)

### Phase 6 · Helpers, file drop, shared terminal
- Helper role: promote / demote, RLS for helper-created sessions, `[helper]` on cards
- Chunked P2P file drop with manifests, resume, group seeding
- `/share` shared terminal with per-typist command tagging

### Phase 7 · Comfort
- Desktop notifications via `notify-send` for cards, DMs and mentions when the TUI is not focused
- Themes in `~/.config/pairdesk/themes/`, `/theme`
- `/search` over a local SQLite full-text index of decrypted history (server only has ciphertext)

### Phase 8 · Release
- GitHub Actions + GoReleaser, signed releases, `--update`
- Landing page + install script on GitHub Pages at `pairdesk.is-a.dev`
- Reduced motion, themes, docs, demo GIF

---

## 11. Feature list

**Social (everyone)**
- Unique usernames, live availability check
- Friend requests, accept / deny, unfriend
- Encrypted DMs with typing indicator and reactions
- Invite-only encrypted groups with owner controls
- Friends pane with filled / hollow online dots, group online counts
- Fingerprint verification

**Admin**
- DM anyone, friend or not
- Badge toggle: hidden by default in chats, always `admin` on broadcasts, always shown on access requests
- Registration switch (open / closed / invite / approval), invite codes, user cap
- Everyone pane with online dots, inspect, ban / unban, kick, mute, reset-key, rename
- Group oversight: list, dissolve, transfer (never read content)
- Announcements, maintenance mode, minimum client version, rate limits, stats, export
- Two-factor admin (login + approved device), admin passphrase, immutable admin log, panic revoke-all
- Terminal watch and board on friends, per-request or under a standing grant
- Screen view and control on friends, always per-request
- Session audit log

**Session logs (host-owned)**
- Every session recorded on the host: who, when, via grant or card
- Full timed transcript with replay
- Each command with exit code and its exact output
- Every file opened / created / modified / deleted, correlated to the command, with diffs for text
- Hash-chained, signed by both sides, `/logs verify`
- Never leaves the machine; visitor's shell cannot reach the log directory

**Sharing**
- `/send`: chunked P2P file drop, resumable, group seeding, no server storage
- `/share`: shared terminal inside a board session
- Helper role for trusted users

**Comfort**
- Desktop notifications, themes
- Full-text search over local decrypted history

**User-side control over the admin**
- Approve / deny every request; 60-second auto-deny
- `/grant` and `/revoke` standing terminal access; watch-only option
- Sudo tiers: blocked by default, ask (admin knows the password), auto (keyring, host away)
- `/stop` ends any session instantly; logging runs from acceptance to end
- Always-visible host bar with one-key kick during any session
- Grant suspends automatically if the admin's key changes

**Platform**
- Single static binary, `curl | sh` install, self-update
- Nothing listens; outbound only
- Four-layer encryption; server stores ciphertext only
- Animated TUI with reduced-motion switch
- Free: Supabase, STUN, TURN quota, GitHub

---

## 12. Add-ons for later

- **Voice call** between admin and a user over the same WebRTC path (audio track, Opus), later any two friends
- **Timed grants**: `/grant --for 2h`
- **Log export** to a signed tarball for sharing a session record with a third party
- **Screen session recording** (full video, opt-in, large)
- **Browser viewer** for screen sessions, view only
- **Multiple instances**: anyone can run their own PairDesk backend from the repo with their own admin
- **Multi-admin** with quorum for destructive actions

---

## 13. Deployment

| Piece | Where |
|---|---|
| Code | `github.com/Kumarsujay03/pairdesk`, MIT |
| Builds and releases | GitHub Actions + GoReleaser on `v*` tags, linux/amd64 + arm64, signed |
| Landing page + `install.sh` | GitHub Pages at `pairdesk.is-a.dev` |
| Backend | One Supabase free project; migrations and RLS in `supabase/`; admin set by a one-time SQL statement |
| Keep-alive | Daily GitHub Actions `curl` |
| Updates | `pairdesk --update`, checksum verified against the GitHub Release |

Supabase URL and anon key are baked in with `-ldflags`. They are public by design; RLS is the gate.
Because the repo is open source, anyone can also spin up their *own* instance with themselves as admin.

---

## 14. Repo layout

```
pairdesk/
├── cmd/pairdesk/          main, flags, --update
├── internal/
│   ├── tui/               chat, friends, users, groups, cards, session pane, animations
│   ├── social/            usernames, friendships, groups
│   ├── crypto/            identity, pairwise + group keys, noise handshake
│   ├── grants/            signed grants, local store, enforcement
│   ├── rendezvous/        Supabase auth, REST, realtime
│   ├── p2p/               pion wrapper, STUN/TURN, channels
│   ├── term/              PTY host and renderer
│   ├── screen/            PipeWire capture, VP8, viewer launcher
│   ├── input/             uinput
│   ├── sessionlog/        transcript, command hooks, file watcher, hash chain, viewer
│   ├── transfer/          chunked P2P file drop, manifests, seeding
│   ├── search/            local SQLite FTS index over decrypted history
│   ├── notify/            notify-send
│   └── audit/             admin-side audit
├── viewer/                SDL screen viewer
├── supabase/              migrations, RLS, presence policies, seed
├── web/                   landing page + install.sh
├── .github/workflows/     release.yml, keepalive.yml
└── PAIRDESK.md            this document
```

---

## 15. Open decisions

- **Username changes:** allow once per 30 days, or never? Lean: once, with old name reserved for 30 days.
- **Who can see your username:** anyone with the exact string (needed for `/add`), but no browsing or search for non-admins. Lean: exact match only.
- **Viewer toolkit:** SDL2.
- **TURN provider:** decide in Phase 4 when a symmetric NAT shows up.
- **Daemon mode** so a grant works while the host's chat is closed: not in v1.
- **Wayland portal consent:** first screen share also triggers the system portal picker. Document it.

---

## 16. Implementation notes (what the code does today)

The build follows this document with these concrete choices, each made for reliability:

- **Terminal sessions take over the whole terminal**, like `ssh`, instead of rendering in a pane. The
  TUI releases the terminal for the session and restores it afterwards. Detach with `ctrl+]` then `q`;
  inside a board session `ctrl+]` then `s` asks the host to share. A pane would need a full terminal
  emulator and would break programs like `vim` and `htop`; raw takeover does not.
- **Screen share** runs as a separate GStreamer window (VP8 over WebRTC, PipeWire via the desktop
  portal, X11 fallback). In control mode the visitor's keyboard is forwarded from the terminal with a
  pointer mode (`ctrl+]` `p`; arrows move, enter clicks). A native window with pointer capture is a
  later improvement. Input injection uses `uinput`, so the host must be in the `input` group.
- **Sudo shim**: the visitor shell gets a `sudo` wrapper first in PATH that always asks the host
  client over a private FIFO. Tier *blocked*: the host sees a card with the exact command and types
  their own password locally (fed to real `sudo` through an askpass helper, never shown to the
  visitor). Tier *ask*: real sudo prompts in the visitor's terminal, the prompt is redacted from the
  transcript. Tier *auto*: the host's stored password is supplied by the askpass helper. What the shim
  does not do: it cannot stop a visitor who already knows the host's password from running
  `/usr/bin/sudo` directly, and because visitor and shell are the same Unix user the shim is a
  policy and logging layer, not a sandbox. The log records every sudo invocation either way.
- **Session logs** live in `~/.local/state/pairdesk/sessions/<id>/` as asciicast + JSONL with a
  per-line hash chain, sealed and signed by the device key. `/logs` lists, shows commands with their
  output, lists touched files with diffs, replays, verifies and deletes. The log and config dirs are
  excluded from the file watcher; the shim dir is 0700.
- **Admin second factor** is a device token minted by the database on first admin run and sent as a
  request header; `is_admin()` checks it on every privileged query. Additional admin devices are
  approved from an existing one. The admin passphrase (Argon2id, local) gates admin and remote
  commands for 15 minutes per unlock (`/admin pass`, `/admin unlock`, `/admin lock`).
- **File drop** is chunked over a WebRTC data channel with per-chunk hashes, resumable, and group
  files are fetched rarest-first from every online member who has announced a copy (`/send #group
  path`, then `/get <id>`).
- **Search** runs over a local history file encrypted with a key derived from the device identity.
- **Notifications** use `notify-send` and are suppressed for the conversation on screen. Sound was
  dropped by design.
- **Demo mode** (`pairdesk --demo`) runs the whole client against an in-memory backend in one
  process, with OTP `000000`, for trying the UI without a Supabase project.
