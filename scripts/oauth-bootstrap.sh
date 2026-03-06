#!/usr/bin/env bash
set -euo pipefail

# Gmail OAuth bootstrap helper for this relay.
# Generates consent URL, exchanges auth code, and prints env values.

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Missing required command: $1" >&2
    exit 1
  fi
}

prompt_if_empty() {
  local var_name="$1"
  local prompt_text="$2"
  local secret="${3:-false}"
  local value="${!var_name:-}"
  if [ -n "$value" ]; then
    return 0
  fi
  if [ "$secret" = "true" ]; then
    read -r -s -p "$prompt_text: " value
    echo
  else
    read -r -p "$prompt_text: " value
  fi
  if [ -z "$value" ]; then
    echo "$var_name is required." >&2
    exit 1
  fi
  printf -v "$var_name" "%s" "$value"
}

urlencode() {
  python3 -c 'import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1], safe=""))' "$1"
}

main() {
  require_cmd curl
  require_cmd python3

  local auth_endpoint token_endpoint scopes redirect_uri_encoded scopes_encoded consent_url
  auth_endpoint="https://accounts.google.com/o/oauth2/v2/auth"
  token_endpoint="https://oauth2.googleapis.com/token"
  scopes="https://www.googleapis.com/auth/gmail.send https://www.googleapis.com/auth/gmail.settings.basic"

  # Optional inputs can be exported before running:
  # GMAIL_CLIENT_ID, GMAIL_CLIENT_SECRET, GMAIL_OAUTH_REDIRECT_URI, GMAIL_MAILBOX
  prompt_if_empty GMAIL_CLIENT_ID "Enter OAuth Client ID"
  prompt_if_empty GMAIL_CLIENT_SECRET "Enter OAuth Client Secret" true
  prompt_if_empty GMAIL_OAUTH_REDIRECT_URI "Enter OAuth Redirect URI (example: http://localhost:8080)"
  prompt_if_empty GMAIL_MAILBOX "Enter Gmail mailbox address"

  redirect_uri_encoded="$(urlencode "$GMAIL_OAUTH_REDIRECT_URI")"
  scopes_encoded="$(urlencode "$scopes")"

  consent_url="${auth_endpoint}?client_id=$(urlencode "$GMAIL_CLIENT_ID")&redirect_uri=${redirect_uri_encoded}&response_type=code&scope=${scopes_encoded}&access_type=offline&prompt=consent&include_granted_scopes=true"

  echo
  echo "1) Open this URL in a browser and grant consent:"
  echo "$consent_url"
  echo
  echo "2) Copy the authorization code from the redirect response."
  local auth_code
  read -r -p "Paste authorization code: " auth_code
  if [ -z "$auth_code" ]; then
    echo "Authorization code is required." >&2
    exit 1
  fi

  local response
  response="$(curl -sS -X POST "$token_endpoint" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    --data "code=$(urlencode "$auth_code")" \
    --data "client_id=$(urlencode "$GMAIL_CLIENT_ID")" \
    --data "client_secret=$(urlencode "$GMAIL_CLIENT_SECRET")" \
    --data "redirect_uri=$(urlencode "$GMAIL_OAUTH_REDIRECT_URI")" \
    --data "grant_type=authorization_code")"

  local refresh_token access_token error_desc
  refresh_token="$(python3 -c 'import json,sys; d=json.loads(sys.stdin.read()); print(d.get("refresh_token",""))' <<<"$response")"
  access_token="$(python3 -c 'import json,sys; d=json.loads(sys.stdin.read()); print(d.get("access_token",""))' <<<"$response")"
  error_desc="$(python3 -c 'import json,sys; d=json.loads(sys.stdin.read()); print(d.get("error_description") or d.get("error",""))' <<<"$response")"

  if [ -z "$refresh_token" ]; then
    echo "Token exchange did not return a refresh token." >&2
    if [ -n "$error_desc" ]; then
      echo "Error: $error_desc" >&2
    fi
    echo "Hint: Ensure access_type=offline and prompt=consent, and verify OAuth app policy in Workspace." >&2
    exit 1
  fi

  echo
  echo "OAuth bootstrap succeeded."
  if [ -n "$access_token" ]; then
    echo "Access token returned (truncated): ${access_token:0:20}..."
  fi
  echo
  echo "Add the following to your .env:"
  echo "GMAIL_CLIENT_ID=${GMAIL_CLIENT_ID}"
  echo "GMAIL_CLIENT_SECRET=${GMAIL_CLIENT_SECRET}"
  echo "GMAIL_REFRESH_TOKEN=${refresh_token}"
  echo "GMAIL_OAUTH_REDIRECT_URI=${GMAIL_OAUTH_REDIRECT_URI}"
  echo "GMAIL_MAILBOX=${GMAIL_MAILBOX}"
  echo
  echo "Security: clear shell history and rotate token immediately if leaked."
}

main "$@"
