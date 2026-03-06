# SMTP Behavior Contract (Phase 1)

## Scope

This document defines the expected external SMTP behavior for the Go relay service.
It is the source of truth for protocol handling, extension advertisement, reply codes,
security posture, and environment-based runtime configuration.

## Service Modes and Ports

- `465` (implicit TLS submission):
  - TLS handshake on connect.
  - No plaintext SMTP accepted.
- `587` (submission with STARTTLS):
  - Plaintext greeting allowed.
  - `STARTTLS` must complete before `AUTH` and before accepting a mail transaction.

## SMTP Command Contract

Supported commands:

- `EHLO` and `HELO`
- `AUTH` (`PLAIN`, `LOGIN`)
- `MAIL FROM`
- `RCPT TO`
- `DATA`
- `RSET`
- `NOOP`
- `QUIT`
- `STARTTLS` (port `587` only, before TLS)

Unsupported commands:

- `VRFY`, `EXPN`, and other unsupported verbs return deterministic not-supported responses.

State machine requirements:

- A client must issue `EHLO` or `HELO` before `MAIL FROM`.
- `MAIL FROM` must succeed before any `RCPT TO`.
- At least one valid `RCPT TO` is required before `DATA`.
- `RSET` clears the current envelope (`MAIL FROM`, `RCPT TO`, data state).
- After successful `DATA`, envelope state resets for next transaction.

## Extension Advertisement Contract

### Before TLS (port 587)

- Advertise `STARTTLS`.
- Do not advertise `AUTH` before TLS.
- Advertise `SIZE` with configured max bytes.
- `PIPELINING` may be advertised only if command ordering is strictly enforced by the server state machine.

### After TLS (port 465 and port 587 post-STARTTLS)

- Advertise `AUTH PLAIN LOGIN`.
- Advertise `SIZE`.
- Advertise `8BITMIME`.
- `SMTPUTF8` and `CHUNKING` are not enabled in Phase 1 and must be rejected deterministically when requested.

## Reply Code Contract

Common success codes:

- `220`: service ready greeting.
- `221`: session closing.
- `235`: authentication successful.
- `250`: command accepted.
- `354`: start mail input.

Common reject and failure codes:

- `421`: service unavailable or load-shedding.
- `451`: transient local failure (retryable).
- `454`: temporary authentication/TLS/backend issue.
- `502` or `504`: command not implemented or parameter not implemented.
- `530`: authentication or TLS required by policy.
- `535`: authentication failed.
- `550` or `553`: sender/recipient/policy validation failure.
- `552`: message size exceeded.
- `554`: permanent transaction rejection.

Determinism rule:

- Same policy failure class must always map to the same SMTP code family and stable message text template.

## Policy and Security Contract

- Sender identity must pass `ALLOWED_SENDER_REGEX`.
- Sender identity must also pass Gmail send-as alias policy before delivery.
- Credentials are loaded from environment-configured auth data.
- Constant-time compare should be used where feasible for password checks.
- Auth attempt controls must include per-connection and per-identity throttling.
- Sensitive values (passwords, OAuth secrets, tokens, auth blobs) must never appear in logs.
- Minimum TLS version is `1.2` (prefer `1.3`).

## Resource and Abuse Controls

Configurable runtime limits:

- Max concurrent connections.
- Max commands per session.
- Max recipients per message.
- Max message size (`SIZE`).
- Idle/read/write timeouts.
- Max line/header length.

Overload behavior:

- Reject early with transient failures (`421` or `451`) instead of exhausting resources.

## Runtime Configuration Contract (Env-First)

Configuration sources:

- Primary source: process environment variables.
- Optional local/development source: `.env` file loaded at startup.
- Precedence: process environment overrides `.env`.

Required keys:

- `SMTP_BIND_ADDR_465`
- `SMTP_BIND_ADDR_587`
- `SMTP_HOSTNAME`
- `QUEUE_DB_PATH`
- `QUEUE_MAX_BACKLOG`
- `SMTP_MAX_MESSAGE_BYTES`
- `SMTP_MAX_RECIPIENTS`
- `SMTP_AUTH_USERS_JSON`
- `ALLOWED_SENDER_REGEX`
- `GMAIL_CLIENT_ID`
- `GMAIL_CLIENT_SECRET`
- `GMAIL_REFRESH_TOKEN`
- `GMAIL_MAILBOX`
- `TLS_CERT_FILE`
- `TLS_KEY_FILE`

Optional keys:

- `SMTP_REQUIRE_TLS_587`
- `SMTP_MAX_CONNECTIONS`
- `SMTP_MAX_COMMANDS_PER_SESSION`
- `SMTP_READ_TIMEOUT_SECONDS`
- `SMTP_WRITE_TIMEOUT_SECONDS`
- `SMTP_IDLE_TIMEOUT_SECONDS`
- `SMTP_AUTH_RATE_LIMIT_PER_MIN`
- `SMTP_AUTH_LOCKOUT_SECONDS`
- `WORKER_POLL_INTERVAL_SECONDS`
- `WORKER_CONCURRENCY`
- `WORKER_MAX_ATTEMPTS`
- `WORKER_RETRY_BASE_SECONDS`
- `WORKER_RETRY_MAX_SECONDS`
- `SENDAS_REFRESH_SECONDS`
- `OBS_HTTP_ADDR`
- `READINESS_ALIAS_MAX_STALE_SECONDS`
- `PROCESSING_STUCK_TIMEOUT_SECONDS`
- `LOG_LEVEL`

### `SMTP_AUTH_USERS_JSON` format

`SMTP_AUTH_USERS_JSON` must be a JSON array of user objects:

```json
[
  {
    "username": "relay-user",
    "password": "plain-text-password-for-now"
  }
]
```

Validation requirements:

- JSON must parse successfully.
- Array must contain at least one user.
- `username` must be non-empty and unique.
- `password` must be non-empty.
- Startup must fail fast on malformed JSON, duplicate usernames, or empty credentials.

Security note:

- Keep `.env` usage to local/dev only.
- In production, inject `SMTP_AUTH_USERS_JSON` from environment/secret manager and avoid writing secrets to disk.

## Acceptance Criteria (Phase 1)

- Behavior contract is documented and versioned in repository.
- Compatibility matrix is documented with explicit pass criteria.
- Reply-code mapping table is complete enough for automated conformance tests.
- Required environment keys are documented and ready for startup validation in Phase 2.
