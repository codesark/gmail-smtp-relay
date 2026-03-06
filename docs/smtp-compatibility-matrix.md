# SMTP Compatibility Matrix (Phase 1)

## Purpose

This matrix defines the compatibility expectations used to validate that the Go relay
preserves required SMTP client behavior.

## Baseline Capability Matrix

| Area | Requirement | Expected Behavior |
|---|---|---|
| Connection | Port 465 | Accept implicit TLS SMTP submission |
| Connection | Port 587 | Support STARTTLS-based submission |
| Greeting | Initial banner | Return `220` with service identity |
| EHLO/HELO | Session start | Accept EHLO/HELO before mail transaction |
| AUTH | PLAIN/LOGIN | Accept only over TLS; return `235` on success |
| MAIL FROM | Sender begin | Enforce state and sender policy |
| RCPT TO | Recipient add | Require valid MAIL state first |
| DATA | Message body | Require at least one accepted recipient |
| RSET | Transaction reset | Clear envelope state |
| NOOP | Keepalive | Return success without side effects |
| QUIT | Session close | Return `221` and close |
| SIZE | Message limit | Advertise and enforce configured max bytes |
| Security | TLS policy | Minimum TLS 1.2, prefer TLS 1.3 |
| Policy | Sender regex | Reject non-matching sender with 5xx |
| Policy | Gmail sendAs | Reject unauthorized sender alias with 5xx |

## Extension Advertisement Matrix

| Port/State | STARTTLS | AUTH | SIZE | 8BITMIME | PIPELINING |
|---|---|---|---|---|---|
| 587 before TLS | Advertised | Not advertised | Advertised | Advertised | Optional |
| 587 after TLS | Not applicable | Advertised | Advertised | Advertised | Optional |
| 465 implicit TLS | Not applicable | Advertised | Advertised | Advertised | Optional |

Notes:

- If `PIPELINING` is advertised, server must still enforce strict command ordering.
- `SMTPUTF8` and `CHUNKING` are not required in Phase 1 and must return deterministic not-supported behavior.

## Reply Code Compatibility Matrix

| Scenario | Expected SMTP Code Family | Notes |
|---|---|---|
| Successful command | 2xx | Use command-specific success code |
| DATA body accepted | 2xx (`250`) | Include queue acceptance semantics |
| Auth success | 2xx (`235`) | Only after TLS |
| Invalid credentials | 5xx (`535`) | Deterministic message template |
| TLS required | 5xx (`530`) | For AUTH/mail preconditions on 587 |
| Unsupported command | 5xx (`502`/`504`) | Deterministic mapping |
| Sender policy fail | 5xx (`550`/`553`) | Regex or alias mismatch |
| Message too large | 5xx (`552`) | Based on configured max bytes |
| Backend transient failure | 4xx (`451`/`454`) | Retryable path |
| Service overload | 4xx (`421`) | Early rejection/load shedding |
| Permanent backend/policy reject | 5xx (`554`) | Non-retryable |

## Env Configuration Compatibility

| Setting Type | Source | Requirement |
|---|---|---|
| Secrets | Process env or `.env` | Mandatory keys must exist at startup |
| Non-secrets | Process env or `.env` | Defaults allowed for selected keys |
| Precedence | Env over `.env` | Explicit env wins |

Auth config requirement:

- `SMTP_AUTH_USERS_JSON` must be valid JSON with unique, non-empty usernames and non-empty passwords.

## Conformance Test Cases (to implement in Phase 2/7)

- `EHLO` then `MAIL`/`RCPT`/`DATA` happy path.
- `AUTH` attempt on 587 before STARTTLS fails with `530`.
- Valid STARTTLS upgrade then AUTH success (`235`).
- Invalid credentials return `535`.
- Sender regex rejection returns 5xx policy error.
- Unauthorized Gmail alias returns 5xx policy error.
- `DATA` without recipients rejected.
- Message size over `SIZE` limit rejected with `552`.
- Unsupported command returns `502/504`.
- Service overload path returns `421`.
