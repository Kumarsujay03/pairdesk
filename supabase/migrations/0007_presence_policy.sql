-- The realtime presence read policy looked at users.banned directly. Regular
-- users are only granted a few columns of users (not banned), so Postgres
-- refused the whole policy, Realtime declined every private presence channel,
-- and nobody ever showed as online. The admin check now goes through a
-- security-definer helper, like the other policies.
create or replace function is_admin_role() returns boolean
language sql stable security definer set search_path = public, extensions as $$
  select exists (select 1 from users where id = auth.uid() and role = 'admin' and not banned)
$$;
revoke execute on function is_admin_role() from public, anon;
grant execute on function is_admin_role() to authenticated;

drop policy if exists realtime_presence_read on realtime.messages;
create policy realtime_presence_read on realtime.messages for select
  to authenticated using (
    realtime.topic() like 'user:%' and (
      realtime.topic() = 'user:' || auth.uid()::text
      or are_friends(auth.uid(), substring(realtime.topic() from 6)::uuid)
      or is_admin_role()
    )
  );
