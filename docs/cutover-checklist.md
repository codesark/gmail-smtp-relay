# Cutover Checklist

## 1) Pre-cutover

- Confirm `.env` is fully populated and secrets are valid.
- Confirm TLS cert/key files exist at configured paths.
- Build and start stack:
  - `docker compose up --build -d`
- Verify service health:
  - `curl -fsS http://127.0.0.1:8080/healthz`
  - `curl -fsS http://127.0.0.1:8080/readyz`
  - `curl -fsS http://127.0.0.1:8080/status`
- Execute local SMTP smoke tests:
  - valid auth + valid sender alias + successful DATA enqueue
  - invalid auth (`535`)
  - invalid sender regex (`550`)
  - invalid alias policy (`550`)
  - queue saturation path (`421`) using reduced `QUEUE_MAX_BACKLOG`

## 2) Parallel validation window

- Run on temporary/non-production published ports first.
- Send controlled canary traffic only.
- Monitor:
  - queue backlog growth/drain (`/status`)
  - worker retries/failures (`/status.worker`)
  - alias cache freshness and errors
- Validate end-to-end Gmail delivery and headers for canary messages.

## 3) Production cutover

- Switch production port mapping/DNS/ingress to Go relay service.
- Keep old relay stack intact but idle for immediate fallback.
- During first hour, watch:
  - auth failure rates
  - `4xx`/`5xx` SMTP response rates
  - queue backlog and oldest pending age
  - Gmail API error trends (transient vs permanent)

## 4) Rollback criteria

Trigger rollback if any persists beyond defined SLO window:

- sustained queue backlog growth with no drain,
- repeated readiness failures,
- elevated permanent send failures,
- widespread client auth/STARTTLS regression.

## 5) Rollback steps

1. Restore previous relay service routing/published ports.
2. Stop Go relay service.
3. Capture diagnostics:
   - `/status` output
   - container logs
   - queue DB snapshot
4. Open incident analysis task with timestamped evidence.

## 6) Post-cutover verification

- Confirm stable health/readiness for at least 24 hours.
- Confirm queue remains near steady-state.
- Confirm expected delivery success rate.
- Record final cutover report and remove temporary toggles/ports.
