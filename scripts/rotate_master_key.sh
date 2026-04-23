#!/usr/bin/env bash
# rotate_master_key.sh — wrapper around cmd/rotate-master-key.
#
# Operational flow for rotating KAPP_MASTER_KEY:
#
#   1. Generate a fresh 32-byte key (openssl rand -base64 32).
#   2. Demote the current key to KAPP_MASTER_KEY_PREV and set the new
#      key as KAPP_MASTER_KEY in your secret manager. Restart all
#      kapp services so the in-process KeyManager picks up both
#      values — DecryptString will try the current key first and fall
#      back to the retiring key, so reads continue to work.
#   3. Run this script against the production DB. It walks every
#      tenant and re-encrypts every encrypted krecords.data string
#      under the new key in batches. Safe to re-run; already-rotated
#      records round-trip without changes.
#   4. Once the script reports `rotate: done`, remove
#      KAPP_MASTER_KEY_PREV from your secret manager and restart
#      services so the KeyManager stops accepting the old key.
#
# Environment variables consumed:
#   DB_URL                — libpq connection string for the app pool
#   KAPP_MASTER_KEY       — current master key (required)
#   KAPP_MASTER_KEY_PREV  — retiring master key (required)
#
# Flags:
#   --batch N             — records per tenant batch (default 200)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

if [[ -z "${DB_URL:-}" ]]; then
    echo "DB_URL is required" >&2
    exit 1
fi
if [[ -z "${KAPP_MASTER_KEY:-}" ]]; then
    echo "KAPP_MASTER_KEY is required" >&2
    exit 1
fi
if [[ -z "${KAPP_MASTER_KEY_PREV:-}" ]]; then
    echo "KAPP_MASTER_KEY_PREV is required (set it to the retiring key)" >&2
    exit 1
fi

cd "$REPO_ROOT"
exec go run ./cmd/rotate-master-key "$@"
