-- bump_rate's parameter was named "bucket", the same as the rate_limits
-- column. Inside the function the "on conflict (user_id, bucket)" clause can
-- mean either, and Postgres refuses with 42702 "column reference bucket is
-- ambiguous". Every rate-limited RPC (send_message, send_friend_request,
-- send_signal) failed with that error. The parameters are renamed; callers
-- pass them positionally, so nothing else changes.
drop function if exists bump_rate(text, int);
create function bump_rate(p_bucket text, p_per_min int) returns boolean
language plpgsql security definer set search_path = public, extensions as $$
declare r rate_limits;
begin
  if p_per_min <= 0 then return true; end if;
  insert into rate_limits(user_id, bucket) values (auth.uid(), p_bucket)
    on conflict (user_id, bucket) do nothing;
  select * into r from rate_limits where user_id = auth.uid() and bucket = p_bucket for update;
  if r.window_start < now() - interval '1 minute' then
    update rate_limits set count = 1, window_start = now() where user_id = auth.uid() and bucket = p_bucket;
    return true;
  end if;
  if r.count >= p_per_min then return false; end if;
  update rate_limits set count = count + 1 where user_id = auth.uid() and bucket = p_bucket;
  return true;
end $$;
-- Stays internal, like before: only the definer functions call it.
revoke execute on function bump_rate(text, int) from public, anon, authenticated;
