-- PairDesk schema. Every table has Row Level Security on; the anon key is
-- public by design and these policies are the only gate. Chat content is
-- ciphertext; the server never holds a key.

create schema if not exists extensions;
create extension if not exists citext with schema extensions;
create extension if not exists pgcrypto with schema extensions;

-- ---------------------------------------------------------------- settings
create table instance_settings (
  key   text primary key,
  value jsonb not null
);
insert into instance_settings(key, value) values
  ('registration',     '"open"'),
  ('require_approval', 'false'),
  ('user_cap',         '0'),
  ('min_version',      '"0.0.0"'),
  ('maintenance',      'false'),
  ('maintenance_msg',  '""'),
  ('announcement',     '""'),
  ('announcement_id',  '""'),
  ('msgs_per_min',     '30'),
  ('admin_pubkey',     '""');

create or replace function setting(k text) returns jsonb
language sql stable security definer set search_path = public, extensions as
$$ select value from instance_settings where key = k $$;

-- ------------------------------------------------------------------- users
create table users (
  id          uuid primary key references auth.users(id) on delete cascade,
  username    citext unique not null check (username ~ '^[a-z0-9_]{3,20}$'),
  pubkey      text not null default '',
  role        text not null default 'user' check (role in ('user','helper','admin')),
  banned      boolean not null default false,
  muted       boolean not null default false,
  muted_until timestamptz,
  approved    boolean not null default true,
  invite_code text,
  kicked_at   timestamptz,
  created_at  timestamptz not null default now(),
  last_seen   timestamptz not null default now()
);
create index users_role_idx on users(role);

-- Admin second factor: a device token hashed in this table, presented as a
-- request header. Email login alone never makes a request "admin".
create table admin_devices (
  device_id   text primary key,
  user_id     uuid not null references users(id) on delete cascade,
  pubkey      text not null,
  token_hash  text,
  approved    boolean not null default false,
  approved_at timestamptz,
  created_at  timestamptz not null default now()
);

create or replace function admin_token() returns text
language sql stable as
$$ select coalesce(current_setting('request.headers', true)::json->>'x-pairdesk-admin-token', '') $$;

create or replace function is_admin() returns boolean
language sql stable security definer set search_path = public, extensions as $$
  select exists (
    select 1 from users u
    join admin_devices d on d.user_id = u.id
    where u.id = auth.uid() and u.role = 'admin' and not u.banned
      and d.approved and d.token_hash = encode(digest(admin_token(), 'sha256'), 'hex')
  )
$$;

-- is_admin_role is the device-token-free check used where no request header
-- exists (realtime policies): admin role and not banned.
create or replace function is_admin_role() returns boolean
language sql stable security definer set search_path = public, extensions as $$
  select exists (select 1 from users where id = auth.uid() and role = 'admin' and not banned)
$$;

create or replace function is_helper_or_admin() returns boolean
language sql stable security definer set search_path = public, extensions as $$
  select is_admin() or exists (
    select 1 from users where id = auth.uid() and role = 'helper' and not banned and approved
  )
$$;

create or replace function is_active() returns boolean
language sql stable security definer set search_path = public, extensions as $$
  select exists (
    select 1 from users where id = auth.uid() and not banned and approved
  )
$$;

create or replace function maintenance_ok() returns boolean
language sql stable as
$$ select (setting('maintenance') = 'false'::jsonb) or is_admin() $$;

-- -------------------------------------------------------------- friendships
create table friendships (
  requester  uuid not null references users(id) on delete cascade,
  addressee  uuid not null references users(id) on delete cascade,
  status     text not null default 'pending' check (status in ('pending','accepted','denied')),
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  primary key (requester, addressee),
  check (requester <> addressee)
);
create unique index friendships_pair_idx on friendships (least(requester, addressee), greatest(requester, addressee));

-- Only answers about the caller (or for the admin), so it is not an oracle.
create or replace function are_friends(a uuid, b uuid) returns boolean
language sql stable security definer set search_path = public, extensions as $$
  select (auth.uid() in (a, b) or is_admin()) and exists (
    select 1 from friendships
    where status = 'accepted'
      and least(requester, addressee) = least(a, b)
      and greatest(requester, addressee) = greatest(a, b)
  )
$$;

-- ------------------------------------------------------------------ groups
create table groups (
  id         uuid primary key default gen_random_uuid(),
  name       text not null check (length(name) between 1 and 40),
  owner      uuid not null references users(id) on delete cascade,
  key_epoch  int  not null default 1,
  created_at timestamptz not null default now()
);

create table group_members (
  group_id    uuid not null references groups(id) on delete cascade,
  user_id     uuid not null references users(id) on delete cascade,
  wrapped_key bytea not null,
  key_epoch   int  not null default 1,
  wrapped_by  uuid not null references users(id),
  added_at    timestamptz not null default now(),
  primary key (group_id, user_id)
);

create or replace function is_member(g uuid, u uuid) returns boolean
language sql stable security definer set search_path = public, extensions as
$$ select (u = auth.uid() or is_admin()) and exists (select 1 from group_members where group_id = g and user_id = u) $$;

create or replace function is_owner(g uuid, u uuid) returns boolean
language sql stable security definer set search_path = public, extensions as
$$ select (u = auth.uid() or is_admin()) and exists (select 1 from groups where id = g and owner = u) $$;

-- ---------------------------------------------------------------- messages
create table messages (
  id         uuid primary key default gen_random_uuid(),
  conv_type  text not null check (conv_type in ('dm','group')),
  conv_id    text not null,
  sender     uuid not null references users(id) on delete cascade,
  ciphertext bytea not null check (length(ciphertext) <= 65536),
  nonce      bytea not null check (length(nonce) = 24),
  key_epoch  int not null default 1,
  created_at timestamptz not null default now()
);
create index messages_conv_idx on messages(conv_type, conv_id, created_at desc);

-- DM conv id is "a:b" with a < b as text.
create or replace function dm_party(conv text, u uuid) returns boolean
language sql immutable as
$$ select split_part(conv, ':', 1) = u::text or split_part(conv, ':', 2) = u::text $$;

create or replace function dm_other(conv text, u uuid) returns uuid
language sql immutable as $$
  select case when split_part(conv, ':', 1) = u::text
              then split_part(conv, ':', 2)::uuid else split_part(conv, ':', 1)::uuid end
$$;

create or replace function can_read_conv(ct text, conv text) returns boolean
language sql stable security definer set search_path = public, extensions as $$
  select case
    when ct = 'dm' then dm_party(conv, auth.uid())
      and split_part(conv, ':', 1) < split_part(conv, ':', 2)
      and (are_friends(auth.uid(), dm_other(conv, auth.uid()))
           or exists (select 1 from users where id in (auth.uid(), dm_other(conv, auth.uid())) and role = 'admin'))
    when ct = 'group' then is_member(conv::uuid, auth.uid())
    else false end
$$;

-- ------------------------------------------------------------------ grants
create table grants (
  id             uuid primary key default gen_random_uuid(),
  grantor        uuid not null references users(id) on delete cascade,
  grantee        uuid not null references users(id) on delete cascade,
  scope          text not null check (scope in ('watch','board')),
  sudo           text not null default 'blocked' check (sudo in ('blocked','ask','auto')),
  grantee_pubkey text not null,
  signature      bytea not null,
  created_at     timestamptz not null default now(),
  revoked_at     timestamptz,
  check (grantor <> grantee)
);
create index grants_pair_idx on grants(grantor, grantee) where revoked_at is null;

-- ------------------------------------------------------------ session logs
create table session_logs (
  id         text primary key check (id ~ '^[A-Za-z0-9_-]{8,32}$'),
  visitor    uuid not null references users(id) on delete cascade,
  host       uuid not null references users(id) on delete cascade,
  kind       text not null check (kind in ('watch','board','screen','share','send')),
  via        text not null check (via in ('grant','card')),
  started_at timestamptz not null default now(),
  ended_at   timestamptz
);

-- ----------------------------------------------------------------- signals
-- Encrypted signaling blobs for P2P setup. Rows are short-lived.
create table signals (
  id         bigserial primary key,
  session_id text not null check (session_id ~ '^[A-Za-z0-9_-]{8,32}$'),
  from_user  uuid not null references users(id) on delete cascade,
  to_user    uuid not null references users(id) on delete cascade,
  type       text not null check (type in ('offer','answer','ice','noise1','noise2','bye','card','card_reply')),
  payload    bytea not null check (length(payload) <= 32768),
  created_at timestamptz not null default now()
);
create index signals_to_idx on signals(to_user, created_at);

-- ----------------------------------------------------------------- invites
create table invites (
  code       text primary key,
  uses_left  int not null,
  expires_at timestamptz,
  created_by uuid not null references users(id),
  created_at timestamptz not null default now(),
  used_by    uuid[] not null default '{}'
);

-- --------------------------------------------------------------- admin log
create table admin_log (
  id     bigserial primary key,
  actor  uuid not null,
  action text not null,
  target text,
  detail text,
  at     timestamptz not null default now()
);

create table rate_limits (
  user_id      uuid not null,
  bucket       text not null,
  count        int not null default 0,
  window_start timestamptz not null default now(),
  primary key (user_id, bucket)
);

-- Pending registrations before a username is chosen live only in auth.users.
