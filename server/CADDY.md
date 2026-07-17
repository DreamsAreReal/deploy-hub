# HTTPS routing — Caddy + Let's Encrypt on sslip.io

Every app is published at a stable HTTPS URL with an automatic Let's Encrypt
certificate, with no domain of our own:

    https://<app>.192-3-94-42.sslip.io  ->  reverse_proxy 127.0.0.1:<port>

`sslip.io` resolves `<anything>.192-3-94-42.sslip.io` back to `192.3.94.42`, so
Let's Encrypt's HTTP-01 challenge on `:80` succeeds and issues a real cert
(verified: issuer `Let's Encrypt`, not Caddy's internal CA).

## Components

- **Caddy** (systemd `caddy`, enabled) owns `:80` and `:443`. Plain `:80`
  redirects to `https` (308). It replaced nginx, which is stopped + disabled.
- **`bin/caddy-sync.sh`** regenerates `/etc/caddy/Caddyfile` from `apps.list`
  (each app's `port=` comes from its `app.conf`) and reloads Caddy. It is
  idempotent: it validates the generated file, writes only on a real change, and
  reloads only when it wrote. `onboard.sh` calls it after registering a new app,
  so a new app gets its route + cert immediately.

## Operations

- Add/refresh all routes: `sudo /opt/deploy-hub/bin/caddy-sync.sh`
- Check a cert issuer:
  `echo | openssl s_client -connect <app>.192-3-94-42.sslip.io:443 \
   -servername <app>.192-3-94-42.sslip.io 2>/dev/null | openssl x509 -issuer -noout`
- Logs: `journalctl -u caddy`

## Firewall

UFW allows `80` and `443`. App container ports stay bound to `127.0.0.1` and are
not reachable externally (Caddy reaches them over loopback, so the DOCKER-USER
barrier that drops external->docker traffic does not interfere).
