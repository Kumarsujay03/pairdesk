# Backend setup

1. Create a free project at supabase.com. Note the project URL and anon key.
2. In the SQL editor run `migrations/0001_schema.sql`, `0002_policies.sql`, `0003_rpc.sql` in order.
3. Enable Realtime for the tables `messages`, `friendships`, `groups`, `group_members`, `grants`,
   `instance_settings`, `signals`, `users` (Database → Replication).
4. Auth → Providers: enable Email with "magic link / OTP". Disable "Confirm email" if you want
   first-launch to be a single OTP step.
5. Build the client with the URL and anon key:
   `go build -ldflags "-X main.supabaseURL=https://xxx.supabase.co -X main.supabaseAnon=eyJ..." ./cmd/pairdesk`
6. The first account to register becomes the admin. On that device run `pairdesk` and it will
   bootstrap the admin device token automatically.
7. Add the GitHub Actions keep-alive secret `SUPABASE_URL` so the daily ping keeps the project awake.
