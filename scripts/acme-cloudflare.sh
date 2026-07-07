#!/usr/bin/env bash
set -euo pipefail

# Let's Encrypt DNS-01 automation using Cloudflare.
# Requires certbot + certbot-dns-cloudflare in the container.

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Missing required command: $1" >&2
    exit 1
  fi
}

normalize_bool() {
  case "${1:-}" in
    1|true|TRUE|yes|YES|on|ON) echo "true" ;;
    *) echo "false" ;;
  esac
}

setup_cloudflare_ini() {
  mkdir -p /tmp/acme
  CF_INI_PATH="/tmp/acme/cloudflare.ini"
  umask 077
  cat >"${CF_INI_PATH}" <<EOF
dns_cloudflare_api_token = ${CF_API_TOKEN}
EOF
  chmod 600 "${CF_INI_PATH}"
}

certbot_base_args() {
  local args
  args=(
    --non-interactive
    --agree-tos
    --email "${LETSENCRYPT_EMAIL}"
    --dns-cloudflare
    --dns-cloudflare-credentials "${CF_INI_PATH}"
    --dns-cloudflare-propagation-seconds "${CF_PROPAGATION_SECONDS}"
    --server "${LETSENCRYPT_DIRECTORY_URL}"
  )
  printf "%s\n" "${args[@]}"
}

ensure_initial_certificate() {
  local live_dir cert_name
  cert_name="${CERT_NAME:-${DNS_NAME}}"
  live_dir="${LETSENCRYPT_CONFIG_DIR}/live/${cert_name}"

  if [ -f "${live_dir}/fullchain.pem" ] && [ -f "${live_dir}/privkey.pem" ]; then
    echo "Existing certificate found for ${cert_name}; skipping initial issue."
    return 0
  fi

  echo "Issuing initial certificate for ${DNS_NAME}..."
  mapfile -t base_args < <(certbot_base_args)
  certbot certonly \
    "${base_args[@]}" \
    --config-dir "${LETSENCRYPT_CONFIG_DIR}" \
    --work-dir "${LETSENCRYPT_WORK_DIR}" \
    --logs-dir "${LETSENCRYPT_LOGS_DIR}" \
    --cert-name "${cert_name}" \
    --keep-until-expiring \
    -d "${DNS_NAME}"
}

copy_certs_for_relay() {
  local cert_name live_dir
  cert_name="${CERT_NAME:-${DNS_NAME}}"
  live_dir="${LETSENCRYPT_CONFIG_DIR}/live/${cert_name}"
  mkdir -p "${CERT_OUTPUT_DIR}"

  cp -f "${live_dir}/fullchain.pem" "${CERT_OUTPUT_DIR}/fullchain.pem"
  cp -f "${live_dir}/privkey.pem" "${CERT_OUTPUT_DIR}/privkey.pem"
  chmod 755 "${CERT_OUTPUT_DIR}"
  chmod 644 "${CERT_OUTPUT_DIR}/fullchain.pem" "${CERT_OUTPUT_DIR}/privkey.pem"
}

run_post_hook_if_any() {
  if [ -n "${ACME_POST_HOOK:-}" ]; then
    echo "Running ACME_POST_HOOK..."
    sh -c "${ACME_POST_HOOK}"
  fi
}

# The relay loads its TLS keypair once at startup, so it must be restarted
# whenever a new certificate is deployed. The deploy-hook drops a flag file;
# if present, restart RELAY_RESTART_CONTAINER through the docker socket
# (python stdlib only — the certbot image has no docker CLI or curl).
restart_relay_if_deployed() {
  [ -f /tmp/acme/cert-deployed ] || return 0
  rm -f /tmp/acme/cert-deployed
  if [ -z "${RELAY_RESTART_CONTAINER:-}" ]; then
    return 0
  fi
  if [ ! -S /var/run/docker.sock ]; then
    echo "docker.sock not mounted; restart ${RELAY_RESTART_CONTAINER} manually to load the new certificate." >&2
    return 0
  fi
  echo "Certificate deployed; restarting ${RELAY_RESTART_CONTAINER}..."
  python3 - "${RELAY_RESTART_CONTAINER}" <<'PY' || echo "Relay restart failed; restart ${RELAY_RESTART_CONTAINER} manually to load the new certificate." >&2
import socket, sys
name = sys.argv[1]
s = socket.socket(socket.AF_UNIX)
s.settimeout(120)
s.connect("/var/run/docker.sock")
s.sendall(f"POST /v1.41/containers/{name}/restart?t=10 HTTP/1.1\r\nHost: docker\r\nContent-Length: 0\r\n\r\n".encode())
status = s.recv(4096).decode().splitlines()[0]
print(f"docker restart {name}: {status}")
sys.exit(0 if " 204" in status else 1)
PY
}

renew_once() {
  mapfile -t base_args < <(certbot_base_args)
  certbot renew \
    "${base_args[@]}" \
    --config-dir "${LETSENCRYPT_CONFIG_DIR}" \
    --work-dir "${LETSENCRYPT_WORK_DIR}" \
    --logs-dir "${LETSENCRYPT_LOGS_DIR}" \
    --keep-until-expiring \
    --deploy-hook "cp -f ${LETSENCRYPT_CONFIG_DIR}/live/${CERT_NAME:-${DNS_NAME}}/fullchain.pem ${CERT_OUTPUT_DIR}/fullchain.pem && cp -f ${LETSENCRYPT_CONFIG_DIR}/live/${CERT_NAME:-${DNS_NAME}}/privkey.pem ${CERT_OUTPUT_DIR}/privkey.pem && chmod 755 ${CERT_OUTPUT_DIR} && chmod 644 ${CERT_OUTPUT_DIR}/fullchain.pem ${CERT_OUTPUT_DIR}/privkey.pem && touch /tmp/acme/cert-deployed"
}

main() {
  require_cmd certbot

  if [ "$(normalize_bool "${ENABLE_ACME_AUTORENEW:-false}")" != "true" ]; then
    echo "ENABLE_ACME_AUTORENEW is false; skipping ACME automation."
    exit 0
  fi

  : "${CF_API_TOKEN:?CF_API_TOKEN is required}"
  : "${DNS_NAME:?DNS_NAME is required}"
  : "${LETSENCRYPT_EMAIL:?LETSENCRYPT_EMAIL is required}"

  LETSENCRYPT_DIRECTORY_URL="${LETSENCRYPT_DIRECTORY_URL:-https://acme-staging-v02.api.letsencrypt.org/directory}"
  CERT_OUTPUT_DIR="${CERT_OUTPUT_DIR:-/certs}"
  CERT_NAME="${CERT_NAME:-${DNS_NAME}}"
  CF_PROPAGATION_SECONDS="${CF_PROPAGATION_SECONDS:-60}"
  RENEW_INTERVAL_SECONDS="${RENEW_INTERVAL_SECONDS:-43200}"
  ACME_RENEW_ONCE="$(normalize_bool "${ACME_RENEW_ONCE:-false}")"
  LETSENCRYPT_CONFIG_DIR="${LETSENCRYPT_CONFIG_DIR:-/etc/letsencrypt}"
  LETSENCRYPT_WORK_DIR="${LETSENCRYPT_WORK_DIR:-/var/lib/letsencrypt}"
  LETSENCRYPT_LOGS_DIR="${LETSENCRYPT_LOGS_DIR:-/var/log/letsencrypt}"

  setup_cloudflare_ini
  mkdir -p "${LETSENCRYPT_CONFIG_DIR}" "${LETSENCRYPT_WORK_DIR}" "${LETSENCRYPT_LOGS_DIR}" "${CERT_OUTPUT_DIR}"

  ensure_initial_certificate
  copy_certs_for_relay
  run_post_hook_if_any

  if [ "${ACME_RENEW_ONCE}" = "true" ]; then
    renew_once
    restart_relay_if_deployed
    run_post_hook_if_any
    exit 0
  fi

  while true; do
    echo "Renew loop: next attempt in ${RENEW_INTERVAL_SECONDS}s."
    sleep "${RENEW_INTERVAL_SECONDS}"
    renew_once || echo "Renew attempt failed; will retry on next interval."
    restart_relay_if_deployed || echo "Relay restart failed; relay may need manual restart to load the new certificate."
    run_post_hook_if_any || echo "Post-hook failed; relay may need manual restart/reload."
  done
}

main "$@"
