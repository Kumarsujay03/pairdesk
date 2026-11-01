-- Clients read instance settings before signing in (registration mode,
-- maintenance notice, minimum version). The settings_read policy already
-- allows everyone; the anon role also needs the table grant.
grant select on instance_settings to anon;
