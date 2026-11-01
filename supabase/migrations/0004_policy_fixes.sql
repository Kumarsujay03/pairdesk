-- Policy fixes found by auditing every client call against the rules in
-- 0002/0003. Safe to run on an existing project; fresh projects run it after
-- 0003 like any other migration.

-- 1. A freshly created group is returned to its creator before the owner's
--    membership row exists; let the owner always read their own groups.
drop policy if exists groups_read on groups;
create policy groups_read on groups for select
  using (owner = auth.uid() or is_member(id, auth.uid()) or is_admin());

-- 2. The owner's own membership row is returned by the insert that creates
--    it; is_member() cannot see that row yet, so allow owners explicitly.
drop policy if exists group_members_read on group_members;
create policy group_members_read on group_members for select
  using (is_member(group_id, auth.uid()) or is_owner(group_id, auth.uid()) or is_admin());

-- 3. users_update_self compared moderation columns through subqueries that
--    run with the caller's privileges, which the column grants deny, so a
--    device could never publish its key. The column-level grant (pubkey,
--    last_seen) already prevents touching anything else.
drop policy if exists users_update_self on users;
create policy users_update_self on users for update
  using (id = auth.uid()) with check (id = auth.uid());

-- 4. Ownership transfer: the new row's owner is the other user, so the old
--    check refused every transfer. Require the new owner to be a member.
drop policy if exists groups_update on groups;
create policy groups_update on groups for update
  using (owner = auth.uid() or is_admin())
  with check (is_admin() or exists (
    select 1 from group_members gm where gm.group_id = groups.id and gm.user_id = groups.owner));

-- 5. A user answering a session card from the admin is not necessarily the
--    admin's friend, and the admin's signal row is deleted on receipt, so
--    the reply was refused. Allow replies addressed to an unbanned admin.
create or replace function send_signal(sid text, target uuid, t text, p bytea) returns bigint
language plpgsql security definer set search_path = public, extensions as $$
declare rid bigint;
begin
  if not is_active() or not maintenance_ok() then raise exception 'forbidden' using errcode = '42501'; end if;
  if not bump_rate('signal', 120) then raise exception 'rate limited' using errcode = 'P0004'; end if;
  if not (
      are_friends(auth.uid(), target)
      or (is_admin() and not exists (select 1 from users where id = target and banned))
      or exists (select 1 from users where id = target and role = 'admin' and not banned)
      or exists (select 1 from signals s where s.session_id = sid and s.from_user = target and s.to_user = auth.uid())
  ) then raise exception 'forbidden' using errcode = '42501'; end if;
  insert into signals(session_id, from_user, to_user, type, payload) values (sid, auth.uid(), target, t, p)
  returning id into rid;
  return rid;
end $$;
