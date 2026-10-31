alter table instance_settings enable row level security;
alter table users             enable row level security;
alter table admin_devices     enable row level security;
alter table friendships       enable row level security;
alter table groups            enable row level security;
alter table group_members     enable row level security;
alter table messages          enable row level security;
alter table grants            enable row level security;
alter table session_logs      enable row level security;
alter table signals           enable row level security;
alter table invites           enable row level security;
alter table admin_log         enable row level security;
alter table rate_limits       enable row level security;

-- settings: everyone reads (clients need registration mode before login);
-- only an admin device writes.
create policy settings_read  on instance_settings for select using (true);
create policy settings_write on instance_settings for update using (is_admin()) with check (is_admin());

-- users: public columns are readable by any signed-in active user for exact
-- lookups; the full list is restricted in the client API by only exposing a
-- by-username RPC. banned/invite_code/kicked_at are admin-only via a view.
create policy users_read_self  on users for select using (id = auth.uid());
create policy users_read_other on users for select using (is_active() and not banned);
create policy users_read_admin on users for select using (is_admin());
create policy users_update_self on users for update
  using (id = auth.uid())
  with check (id = auth.uid() and role = (select role from users where id = auth.uid())
              and banned = (select banned from users where id = auth.uid())
              and muted = (select muted from users where id = auth.uid())
              and approved = (select approved from users where id = auth.uid())
              and username = (select username from users where id = auth.uid()));
create policy users_update_admin on users for update using (is_admin()) with check (is_admin());

-- admin devices: an admin-role user may register a device for themselves;
-- only an already-approved admin device may approve another.
create policy admin_devices_read on admin_devices for select
  using (user_id = auth.uid());
create policy admin_devices_insert on admin_devices for insert
  with check (user_id = auth.uid()
              and exists (select 1 from users where id = auth.uid() and role = 'admin'));
create policy admin_devices_update on admin_devices for update
  using (is_admin()) with check (is_admin());

-- friendships
create policy friendships_read on friendships for select
  using (requester = auth.uid() or addressee = auth.uid() or is_admin());
create policy friendships_insert on friendships for insert
  with check (requester = auth.uid() and is_active() and maintenance_ok()
              and status = 'pending'
              and not exists (select 1 from users where id = addressee and banned));
create policy friendships_respond on friendships for update
  using (addressee = auth.uid() and status = 'pending')
  with check (addressee = auth.uid() and status in ('accepted','denied'));
create policy friendships_delete on friendships for delete
  using (requester = auth.uid() or addressee = auth.uid() or is_admin());

-- groups
create policy groups_read on groups for select
  using (is_member(id, auth.uid()) or is_admin());
create policy groups_insert on groups for insert
  with check (owner = auth.uid() and is_active() and maintenance_ok());
create policy groups_update on groups for update
  using (owner = auth.uid() or is_admin()) with check (true);
create policy groups_delete on groups for delete
  using (owner = auth.uid() or is_admin());

-- group members: owner adds friends only; members may remove themselves.
create policy group_members_read on group_members for select
  using (is_member(group_id, auth.uid()) or is_admin());
create policy group_members_insert on group_members for insert
  with check (is_owner(group_id, auth.uid()) and maintenance_ok()
              and (user_id = auth.uid() or are_friends(auth.uid(), user_id)));
create policy group_members_update on group_members for update
  using (is_owner(group_id, auth.uid())) with check (is_owner(group_id, auth.uid()));
create policy group_members_delete on group_members for delete
  using (user_id = auth.uid() or is_owner(group_id, auth.uid()) or is_admin());

-- messages: party only; admin exception for DMs; inserts go through RPC.
create policy messages_read on messages for select
  using (can_read_conv(conv_type, conv_id));

-- grants: grantor writes, both parties read. Grantee must be admin/helper.
create policy grants_read on grants for select
  using (grantor = auth.uid() or grantee = auth.uid());
create policy grants_insert on grants for insert
  with check (grantor = auth.uid() and is_active() and maintenance_ok()
              and exists (select 1 from users where id = grantee and role in ('admin','helper')));
create policy grants_revoke on grants for update
  using (grantor = auth.uid()) with check (grantor = auth.uid());

-- session logs: either party writes its own rows, both read.
create policy session_logs_read on session_logs for select
  using (visitor = auth.uid() or host = auth.uid() or is_admin());
create policy session_logs_insert on session_logs for insert
  with check ((visitor = auth.uid() or host = auth.uid()) and is_active());
create policy session_logs_update on session_logs for update
  using (visitor = auth.uid() or host = auth.uid());

-- signals: friends may open a session with each other (file drop); the
-- receiving client refuses terminal/screen cards from non-admin/helper
-- senders, and the other party may reply inside a session opened to them.
create policy signals_read on signals for select
  using (to_user = auth.uid());
create policy signals_insert on signals for insert
  with check (from_user = auth.uid() and is_active() and maintenance_ok() and (
      are_friends(auth.uid(), to_user)
      or (is_admin() and not exists (select 1 from users where id = to_user and banned))
      or exists (select 1 from signals s where s.session_id = signals.session_id
                 and s.from_user = to_user and s.to_user = auth.uid())));
create policy signals_delete on signals for delete
  using (to_user = auth.uid() or from_user = auth.uid());

-- invites: admin only
create policy invites_all on invites for all using (is_admin()) with check (is_admin());

-- admin log: insert-only, readable by admin
create policy admin_log_read on admin_log for select using (is_admin());

-- rate limits: nobody touches directly (RPCs are security definer)
create policy rate_limits_none on rate_limits for select using (false);
