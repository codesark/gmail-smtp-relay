#!/usr/bin/env python3
import json
import logging
import os
import sys
import time
import urllib.request
from urllib.error import HTTPError, URLError
import ipaddress

logging.basicConfig(level=logging.INFO, format="%(levelname)s: %(message)s")
logger = logging.getLogger(__name__)

def normalize_bool(val: str) -> bool:
    if not val:
        return False
    return str(val).strip().lower() in ("1", "true", "yes", "on")

def get_env_or_exit(key: str) -> str:
    val = os.environ.get(key)
    if not val:
        logger.error(f"{key} is required")
        sys.exit(1)
    return val

CF_API_TOKEN = get_env_or_exit("CF_API_TOKEN")
DNS_NAME = get_env_or_exit("DNS_NAME")

ENABLE_AUTO_DNS = normalize_bool(os.environ.get("ENABLE_AUTO_DNS", "false"))
CF_ZONE_ID = os.environ.get("CF_ZONE_ID", "")
CF_ZONE_NAME = os.environ.get("CF_ZONE_NAME", "")
CF_TTL = int(os.environ.get("CF_TTL", "300"))
CF_PROXIED = normalize_bool(os.environ.get("CF_PROXIED", "false"))
DRY_RUN = normalize_bool(os.environ.get("DRY_RUN", "false"))
DNS_SYNC_INTERVAL_SECONDS = int(os.environ.get("DNS_SYNC_INTERVAL_SECONDS", "300"))
DNS_SYNC_ONCE = normalize_bool(os.environ.get("DNS_SYNC_ONCE", "false"))

if CF_TTL != 1 and not (60 <= CF_TTL <= 86400):
    logger.error("CF_TTL must be 1 (auto) or between 60 and 86400.")
    sys.exit(1)

def cf_api(method: str, path: str, body: dict = None) -> dict:
    url = f"https://api.cloudflare.com/client/v4{path}"
    headers = {
        "Authorization": f"Bearer {CF_API_TOKEN}",
        "Content-Type": "application/json"
    }
    data = json.dumps(body).encode("utf-8") if body else None
    
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req) as response:
            res_body = response.read().decode("utf-8")
            return json.loads(res_body)
    except HTTPError as e:
        err_body = e.read().decode("utf-8")
        logger.error(f"Cloudflare API HTTPError: {e.code} {e.reason} - {err_body}")
        sys.exit(1)
    except URLError as e:
        logger.error(f"Cloudflare API URLError: {e.reason}")
        sys.exit(1)
    except json.JSONDecodeError:
        logger.error(f"Cloudflare API Invalid JSON response.")
        sys.exit(1)

def resolve_zone_id() -> str:
    if CF_ZONE_ID:
        return CF_ZONE_ID
    if not CF_ZONE_NAME:
        logger.error("CF_ZONE_ID or CF_ZONE_NAME is required.")
        sys.exit(1)
    
    resp = cf_api("GET", f"/zones?name={CF_ZONE_NAME}")
    if not resp.get("success"):
        logger.error("Cloudflare zone lookup failed.")
        sys.exit(1)
    
    results = resp.get("result", [])
    if not results:
        logger.error("Unable to resolve zone id from CF_ZONE_NAME.")
        sys.exit(1)
    return results[0]["id"]

def upsert_record(zone_id: str, record_type: str, record_name: str, record_content: str, proxied: bool, ttl: int):
    list_url = f"/zones/{zone_id}/dns_records?type={record_type}&name={record_name}"
    resp = cf_api("GET", list_url)
    if not resp.get("success"):
        logger.error(f"Cloudflare DNS list failed for {record_type} {record_name}.")
        sys.exit(1)
    
    results = resp.get("result", [])
    record_id = None
    if results:
        current = results[0]
        record_id = current.get("id")
        current_content = current.get("content", "")
        current_proxied = current.get("proxied", False)
        current_ttl = current.get("ttl")
        
        if current_content == record_content and current_proxied == proxied and current_ttl == ttl:
            logger.info(f"No change for {record_type} {record_name} ({record_content}).")
            return

    payload = {
        "type": record_type,
        "name": record_name,
        "content": record_content,
        "proxied": proxied,
        "ttl": ttl
    }
    
    if DRY_RUN:
        action = "update" if record_id else "create"
        logger.info(f"[DRY_RUN] Would {action} {record_type} {record_name} -> {record_content}")
        return

    if record_id:
        cf_api("PUT", f"/zones/{zone_id}/dns_records/{record_id}", payload)
        logger.info(f"Updated {record_type} {record_name} -> {record_content}")
    else:
        cf_api("POST", f"/zones/{zone_id}/dns_records", payload)
        logger.info(f"Created {record_type} {record_name} -> {record_content}")

def detect_ip(url: str, family: str) -> str:
    req = urllib.request.Request(url)
    try:
        with urllib.request.urlopen(req, timeout=10) as response:
            ip = response.read().decode("utf-8").strip()
            if family == "IPv4":
                ipaddress.IPv4Address(ip)
            elif family == "IPv6":
                ipaddress.IPv6Address(ip)
            return ip
    except Exception as e:
        logger.debug(f"Failed to detect {family}: {e}")
        return ""

def run_sync():
    zone_id = resolve_zone_id()
    
    ipv4 = detect_ip("https://api.ipify.org", "IPv4")
    if not ipv4:
        logger.error("Failed to detect public IPv4 address, or address is invalid.")
        sys.exit(1)
    
    ipv6 = detect_ip("https://api64.ipify.org", "IPv6")
    
    upsert_record(zone_id, "A", DNS_NAME, ipv4, CF_PROXIED, CF_TTL)
    if ipv6:
        upsert_record(zone_id, "AAAA", DNS_NAME, ipv6, CF_PROXIED, CF_TTL)
    else:
        logger.info("No valid public IPv6 detected; skipping AAAA update.")

def main():
    if not ENABLE_AUTO_DNS:
        logger.info("ENABLE_AUTO_DNS is false; DNS sync sidecar is idle.")
        while True:
            time.sleep(3600)
    
    run_sync()
    if DNS_SYNC_ONCE:
        sys.exit(0)
    
    while True:
        time.sleep(DNS_SYNC_INTERVAL_SECONDS)
        try:
            run_sync()
        except SystemExit:
            pass
        except Exception as e:
            logger.error(f"Unhandled explicit exception during sync round: {e}")

if __name__ == "__main__":
    main()
