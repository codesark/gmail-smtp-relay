# Cloudflare DNS + ACME Automation

This runbook describes automated DNS updates (A/AAAA) and Let's Encrypt certificate management using Cloudflare DNS-01.

## Services

- `dns-sync`:
  - detects public IPv4/IPv6,
  - upserts `A` + `AAAA` records for `DNS_NAME` in Cloudflare.
- `acme-renew`:
  - requests certificate via DNS-01 challenge,
  - writes cert/key to `/certs/fullchain.pem` and `/certs/privkey.pem`,
  - renews on interval.

Relay service reads:

- `TLS_CERT_FILE=/certs/fullchain.pem`
- `TLS_KEY_FILE=/certs/privkey.pem`

Host-local persistence (bind mounts):

- `./storage/relay-data` -> `/app/data`
- `./storage/certs` -> `/certs`
- `./storage/letsencrypt/config` -> `/etc/letsencrypt`
- `./storage/letsencrypt/work` -> `/var/lib/letsencrypt`
- `./storage/letsencrypt/logs` -> `/var/log/letsencrypt`

## Required env variables

- Cloudflare:
  - `ENABLE_AUTO_DNS=true`
  - `CF_API_TOKEN`
  - `DNS_NAME`
  - `CF_ZONE_ID` (or `CF_ZONE_NAME`)
- ACME:
  - `ENABLE_ACME_AUTORENEW=true`
  - `LETSENCRYPT_EMAIL`
  - `LETSENCRYPT_DIRECTORY_URL`

## Staging-first rollout (recommended)

1. Set staging directory:
   - `LETSENCRYPT_DIRECTORY_URL=https://acme-staging-v02.api.letsencrypt.org/directory`
2. Start stack:
   - `docker compose up -d`
3. Verify DNS sync logs:
   - `docker compose logs -f dns-sync`
4. Verify certificate issuance logs:
   - `docker compose logs -f acme-renew`
5. Confirm cert files exist inside relay:
   - `docker compose exec go-smtp-gmail-relay ls -l /certs`
6. Confirm relay health:
   - `docker compose exec go-smtp-gmail-relay wget -qO- http://127.0.0.1:8080/readyz`

Optional pre-create storage paths:

```bash
mkdir -p storage/relay-data storage/certs storage/letsencrypt/{config,work,logs}
```

After successful staging validation, switch to production:

- `LETSENCRYPT_DIRECTORY_URL=https://acme-v02.api.letsencrypt.org/directory`
- `docker compose up -d`

## Renewal behavior

- Renewal loop interval: `RENEW_INTERVAL_SECONDS` (default 12h).
- Certbot only renews when nearing expiry.
- Optional post action command:
  - `ACME_POST_HOOK` (for example, controlled restart/reload workflow).

## Security guidance

- Use least-privilege Cloudflare token scoped to DNS edit on required zone.
- Do not print/store Cloudflare token in logs.
- Keep `.env` out of git.
- Rotate token immediately if exposure is suspected.

## Rollback

If automation causes instability:

1. Disable automation:
   - `ENABLE_AUTO_DNS=false`
   - `ENABLE_ACME_AUTORENEW=false`
2. Provide known-good cert paths via mounted files and `TLS_CERT_FILE`/`TLS_KEY_FILE`.
3. Recreate services:
   - `docker compose up -d`
4. Capture logs from `dns-sync` and `acme-renew` for incident analysis.

## Backup and restore

Backup:

```bash
tar -czf relay-backup-$(date +%Y%m%d%H%M%S).tar.gz storage/
```

Restore:

```bash
tar -xzf relay-backup-<timestamp>.tar.gz
docker compose up -d
```
