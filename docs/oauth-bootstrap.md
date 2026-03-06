# OAuth Bootstrap Helper

Use [`scripts/oauth-bootstrap.sh`](../scripts/oauth-bootstrap.sh) to generate Gmail OAuth tokens for this relay.

## Prerequisites

- `curl` and `python3` available on your machine.
- Google Cloud OAuth client already created.
- Gmail API enabled in your Google Cloud project.
- Workspace mailbox prepared for relay usage.

## Required scopes

- `https://www.googleapis.com/auth/gmail.send`
- `https://www.googleapis.com/auth/gmail.settings.basic`

## Run helper

```bash
chmod +x scripts/oauth-bootstrap.sh
./scripts/oauth-bootstrap.sh
```

You can pre-provide values:

```bash
export GMAIL_CLIENT_ID="..."
export GMAIL_CLIENT_SECRET="..."
export GMAIL_OAUTH_REDIRECT_URI="http://localhost:8080"
export GMAIL_MAILBOX="relay-mailbox@example.com"
./scripts/oauth-bootstrap.sh
```

The helper will:

1. Print a consent URL.
2. Ask for authorization code.
3. Exchange code at Google token endpoint.
4. Print `.env`-ready values:
   - `GMAIL_CLIENT_ID`
   - `GMAIL_CLIENT_SECRET`
   - `GMAIL_REFRESH_TOKEN`
   - `GMAIL_OAUTH_REDIRECT_URI`
   - `GMAIL_MAILBOX`

## Troubleshooting

- No refresh token returned:
  - Ensure first consent uses `access_type=offline` and `prompt=consent`.
  - Revoke existing grant and re-consent if needed.
- `redirect_uri_mismatch`:
  - Ensure redirect URI exactly matches OAuth client config.
- `invalid_client`:
  - Verify client ID/secret and OAuth client type.
- Workspace app blocked:
  - In Workspace admin controls, trust/allow the OAuth app.

## Security notes

- Do not commit token/secret values to git.
- Prefer secret manager injection in production.
- Use a dedicated mailbox and least-privilege scopes.
- Revoke and rotate refresh token immediately if exposed.
