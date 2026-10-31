-- Server-side functions. These are the only write paths that need logic
-- beyond a policy: registration, sending messages (rate limit + mute),
-- and admin actions (which must also log).

create or replace function admin_log_write(a text, t text, d text) returns void
language sql security definer set search_path = public as
$$ insert into admin_log(actor, action, target, detail) values (auth.uid(), a, t, d) $$;

-- Registration -------------------------------------------------------------
create or replace function check_username(name text) returns boolean
language sql stable security definer set search_path = public as
$$ select not exists (select 1 from users where username = name) and name ~ '^[a-z0-9_]{3,20}$' $$;

create or replace function register_username(name text, pub text, invite text default null)
returns users language plpgsql security definer set search_path = public as $$
declare
  mode text := setting('registration') #>> '{}';
  cap int := (setting('user_cap') #>> '{}')::int;
  need_approval boolean := setting('require_approval') = 'true'::jsonb;
  first boolean := not exists (select 1 from users);
  cnt int;
  inv invites;
  u users;
begin
  if auth.uid() is null then raise exception 'unauthorized' using errcode = '42501'; end if;
  perform pg_advisory_xact_lock(7340101);
  first := not exists (select 1 from users);
  if exists (select 1 from users where id = auth.uid()) then
    raise exception 'already registered' using errcode = '23505';
  end if;
  if not first then
    if mode = 'closed' then raise exception 'registration closed' using errcode = 'P0001'; end if;
    select count(*) into cnt from users;
    if cap > 0 and cnt >= cap then raise exception 'registration closed' using errcode = 'P0001'; end if;
    if mode = 'invite' then
      select * into inv from invites where code = invite for update;
      if inv.code is null or inv.uses_left <= 0 or (inv.expires_at is not null and inv.expires_at < now()) then
        raise exception 'invalid invite' using errcode = 'P0002';
      end if;
      update invites set uses_left = uses_left - 1, used_by = used_by || auth.uid() where code = invite;
    end if;
  end if;
  insert into users(id, username, pubkey, role, approved, invite_code)
  values (auth.uid(), name, pub,
          case when first then 'admin' else 'user' end,
          case when first then true else not need_approval end,
          invite)
  returning * into u;
  if first then
    update instance_settings set value = to_jsonb(pub) where key = 'admin_pubkey';
  end if;
  return u;
end $$;

-- Messages -------------------------------------------------------------------
create or replace function bump_rate(bucket text, per_min int) returns boolean
language plpgsql security definer set search_path = public as $$
declare r rate_limits;
begin
  if per_min <= 0 then return true; end if;
  insert into rate_limits(user_id, bucket) values (auth.uid(), bucket)
    on conflict (user_id, bucket) do nothing;
  select * into r from rate_limits where user_id = auth.uid() and rate_limits.bucket = bump_rate.bucket for update;
  if r.window_start < now() - interval '1 minute' then
    update rate_limits set count = 1, window_start = now() where user_id = auth.uid() and rate_limits.bucket = bump_rate.bucket;
    return true;
  end if;
  if r.count >= per_min then return false; end if;
  update rate_limits set count = count + 1 where user_id = auth.uid() and rate_limits.bucket = bump_rate.bucket;
  return true;
end $$;

create or replace function send_message(ct text, conv text, cipher bytea, n bytea, epoch int)
returns messages language plpgsql security definer set search_path = public as $$
declare m messages; me users;
begin
  select * into me from users where id = auth.uid();
  if me.id is null or me.banned or not me.approved then raise exception 'forbidden' using errcode = '42501'; end if;
  if not maintenance_ok() then raise exception 'maintenance' using errcode = 'P0003'; end if;
  if me.muted and (me.muted_until is null or me.muted_until > now()) then
    raise exception 'muted' using errcode = '42501';
  end if;
  if not can_read_conv(ct, conv) then raise exception 'forbidden' using errcode = '42501'; end if;
  if not bump_rate('msg', (setting('msgs_per_min') #>> '{}')::int) then
    raise exception 'rate limited' using errcode = 'P0004';
  end if;
  insert into messages(conv_type, conv_id, sender, ciphertext, nonce, key_epoch)
  values (ct, conv, auth.uid(), cipher, n, epoch) returning * into m;
  return m;
end $$;

-- Friend requests with denial cooldown ---------------------------------------
create or replace function send_friend_request(target uuid) returns void
language plpgsql security definer set search_path = public as $$
declare f friendships;
begin
  if not is_active() or not maintenance_ok() then raise exception 'forbidden' using errcode = '42501'; end if;
  if not bump_rate('friend', 20) then raise exception 'rate limited' using errcode = 'P0004'; end if;
  select * into f from friendships
   where least(requester, addressee) = least(auth.uid(), target)
     and greatest(requester, addressee) = greatest(auth.uid(), target);
  if f.requester is not null then
    if f.status = 'accepted' then return; end if;
    if f.status = 'denied' and f.updated_at > now() - interval '24 hours' then
      raise exception 'cooldown' using errcode = '23505';
    end if;
    if f.status = 'pending' and f.requester = target then
      update friendships set status = 'accepted', updated_at = now() where requester = f.requester and addressee = f.addressee;
      return;
    end if;
    delete from friendships where requester = f.requester and addressee = f.addressee;
  end if;
  insert into friendships(requester, addressee) values (auth.uid(), target);
end $$;

-- Group key rotation: replace every member's wrapped key atomically ---------
create or replace function rotate_group_key(g uuid, new_epoch int, members jsonb) returns void
language plpgsql security definer set search_path = public as $$
declare m jsonb;
begin
  if not is_owner(g, auth.uid()) then raise exception 'forbidden' using errcode = '42501'; end if;
  for m in select * from jsonb_array_elements(members) loop
    update group_members
       set wrapped_key = decode(m->>'wrapped_key', 'base64'), key_epoch = new_epoch, wrapped_by = auth.uid()
     where group_id = g and user_id = (m->>'user_id')::uuid;
  end loop;
  update groups set key_epoch = new_epoch where id = g;
end $$;

-- Signaling (rate limited) ----------------------------------------------------
create or replace function send_signal(sid text, target uuid, t text, p bytea) returns bigint
language plpgsql security definer set search_path = public as $$
declare rid bigint;
begin
  if not is_active() or not maintenance_ok() then raise exception 'forbidden' using errcode = '42501'; end if;
  if not bump_rate('signal', 120) then raise exception 'rate limited' using errcode = 'P0004'; end if;
  if not (
      are_friends(auth.uid(), target)
      or (is_admin() and not exists (select 1 from users where id = target and banned))
      or exists (select 1 from signals s where s.session_id = sid and s.from_user = target and s.to_user = auth.uid())
  ) then raise exception 'forbidden' using errcode = '42501'; end if;
  insert into signals(session_id, from_user, to_user, type, payload) values (sid, auth.uid(), target, t, p)
  returning id into rid;
  return rid;
end $$;

-- Own full row (moderation columns are hidden from the table grant).
create or replace function me_detail() returns jsonb
language sql stable security definer set search_path = public as $$
  select to_jsonb(u) from users u where u.id = auth.uid()
$$;

-- Admin-only full user row (moderation columns are hidden from the table grant).
create or replace function admin_user_detail(target uuid) returns jsonb
language sql stable security definer set search_path = public as $$
  select case when is_admin() then to_jsonb(u) else null end from users u where u.id = target
$$;

create or replace function admin_list_users() returns setof users
language sql stable security definer set search_path = public as $$
  select * from users where is_admin()
$$;

-- Admin ----------------------------------------------------------------------
create or replace function admin_set_setting(k text, v jsonb) returns void
language plpgsql security definer set search_path = public as $$
begin
  if not is_admin() then raise exception 'forbidden' using errcode = '42501'; end if;
  update instance_settings set value = v where key = k;
  perform admin_log_write('set_setting', k, v::text);
end $$;

create or replace function admin_set_user(target uuid, flag text, val boolean) returns void
language plpgsql security definer set search_path = public as $$
begin
  if not is_admin() then raise exception 'forbidden' using errcode = '42501'; end if;
  if flag = 'banned' then update users set banned = val where id = target;
  elsif flag = 'muted' then update users set muted = val where id = target;
  elsif flag = 'approved' then update users set approved = val where id = target;
  else raise exception 'bad flag'; end if;
  perform admin_log_write('set_user_' || flag, target::text, val::text);
end $$;

create or replace function admin_set_role(target uuid, r text) returns void
language plpgsql security definer set search_path = public as $$
begin
  if not is_admin() then raise exception 'forbidden' using errcode = '42501'; end if;
  if r not in ('user','helper') then raise exception 'bad role'; end if;
  update users set role = r where id = target and role <> 'admin';
  perform admin_log_write('set_role', target::text, r);
end $$;

create or replace function admin_rename(target uuid, newname text) returns void
language plpgsql security definer set search_path = public as $$
begin
  if not is_admin() then raise exception 'forbidden' using errcode = '42501'; end if;
  update users set username = newname where id = target;
  perform admin_log_write('rename', target::text, newname);
end $$;

create or replace function admin_reset_key(target uuid) returns void
language plpgsql security definer set search_path = public as $$
begin
  if not is_admin() then raise exception 'forbidden' using errcode = '42501'; end if;
  update users set pubkey = '' where id = target;
  update grants set revoked_at = now() where (grantor = target or grantee = target) and revoked_at is null;
  perform admin_log_write('reset_key', target::text, null);
end $$;

create or replace function admin_kick(target uuid) returns void
language plpgsql security definer set search_path = public as $$
begin
  if not is_admin() then raise exception 'forbidden' using errcode = '42501'; end if;
  update users set kicked_at = now() where id = target;
  perform admin_log_write('kick', target::text, null);
end $$;

create or replace function admin_create_invite(uses int, expires timestamptz) returns invites
language plpgsql security definer set search_path = public as $$
declare i invites;
begin
  if not is_admin() then raise exception 'forbidden' using errcode = '42501'; end if;
  insert into invites(code, uses_left, expires_at, created_by)
  values (encode(gen_random_bytes(6), 'hex'), uses, expires, auth.uid()) returning * into i;
  perform admin_log_write('create_invite', i.code, uses::text);
  return i;
end $$;

create or replace function admin_approve_device(dev text) returns text
language plpgsql security definer set search_path = public as $$
declare tok text := encode(gen_random_bytes(32), 'hex');
begin
  if not is_admin() then raise exception 'forbidden' using errcode = '42501'; end if;
  update admin_devices set approved = true, approved_at = now(),
         token_hash = encode(digest(tok, 'sha256'), 'hex') where device_id = dev;
  perform admin_log_write('approve_device', dev, null);
  return tok;
end $$;

-- Bootstrap: the very first admin device self-approves. Only works while the
-- admin has zero approved devices.
create or replace function admin_bootstrap_device(dev text, pub text) returns text
language plpgsql security definer set search_path = public as $$
declare tok text := encode(gen_random_bytes(32), 'hex');
begin
  if not exists (select 1 from users where id = auth.uid() and role = 'admin') then
    raise exception 'forbidden' using errcode = '42501';
  end if;
  if exists (select 1 from admin_devices where user_id = auth.uid() and approved) then
    raise exception 'already bootstrapped' using errcode = '42501';
  end if;
  insert into admin_devices(device_id, user_id, pubkey, approved, approved_at, token_hash)
  values (dev, auth.uid(), pub, true, now(), encode(digest(tok, 'sha256'), 'hex'));
  perform admin_log_write('bootstrap_device', dev, null);
  return tok;
end $$;

create or replace function admin_stats() returns jsonb
language sql stable security definer set search_path = public as $$
  select case when is_admin() then jsonb_build_object(
    'users_total',   (select count(*) from users),
    'users_today',   (select count(*) from users where last_seen > now() - interval '1 day'),
    'msgs_today',    (select count(*) from messages where created_at > now() - interval '1 day'),
    'sessions_week', (select count(*) from session_logs where started_at > now() - interval '7 days'),
    'db_bytes',      pg_database_size(current_database())
  ) else null end
$$;

create or replace function touch_last_seen() returns void
language sql security definer set search_path = public as
$$ update users set last_seen = now() where id = auth.uid() $$;

-- Housekeeping: drop old signals and expired denials.
create or replace function housekeeping() returns void
language sql security definer set search_path = public as $$
  delete from signals where created_at < now() - interval '10 minutes';
  delete from friendships where status = 'pending' and created_at < now() - interval '7 days';
$$;

-- Realtime authorization for private channels (presence + broadcast).
-- Topic "user:<uuid>" may be read by that user, their friends, or admin.
create policy realtime_presence_read on realtime.messages for select
  to authenticated using (
    realtime.topic() like 'user:%' and (
      realtime.topic() = 'user:' || auth.uid()::text
      or are_friends(auth.uid(), substring(realtime.topic() from 6)::uuid)
      or exists (select 1 from users where id = auth.uid() and role = 'admin' and not banned)
    )
  );
create policy realtime_presence_write on realtime.messages for insert
  to authenticated with check (
    realtime.topic() = 'user:' || auth.uid()::text
  );

-- Functions are PUBLIC-executable by default in Postgres; take that away and
-- grant only the client-facing RPCs to authenticated users.
revoke execute on all functions in schema public from public, anon, authenticated;
grant execute on function
  setting(text), is_admin(), is_helper_or_admin(), is_active(), maintenance_ok(),
  check_username(text), register_username(text, text, text), touch_last_seen(),
  send_message(text, text, bytea, bytea, int), send_friend_request(uuid),
  rotate_group_key(uuid, int, jsonb), send_signal(text, uuid, text, bytea),
  can_read_conv(text, text),
  admin_set_setting(text, jsonb), admin_set_user(uuid, text, boolean), admin_set_role(uuid, text),
  admin_rename(uuid, text), admin_reset_key(uuid), admin_kick(uuid),
  admin_create_invite(int, timestamptz), admin_approve_device(text), admin_bootstrap_device(text, text),
  admin_stats(), admin_user_detail(uuid), admin_list_users(), me_detail(),
  are_friends(uuid, uuid), is_member(uuid, uuid), is_owner(uuid, uuid)
to authenticated;
-- are_friends / is_member / is_owner / dm_* / bump_rate / admin_log_write / housekeeping stay
-- internal: callable only from the definer functions and policies above.
alter default privileges in schema public revoke execute on functions from public;
