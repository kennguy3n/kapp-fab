#!/usr/bin/env bash
# docker-prod-setup.sh — interactive bootstrap for the production stack
# defined in docker-compose.prod.yml (Workstream 4, NoOps Infrastructure).
#
# What it does, idempotently:
#   1. Generates strong random secrets for KAPP_JWT_SECRET,
#      KAPP_MASTER_KEY, the DB role passwords, the ZK fabric admin token,
#      and Stripe placeholder keys (only for values not already present).
#   2. Prompts for the deployment-specific values (public domain, ACME
#      email, KChat base URL / API key, first platform admin) — pre-filled
#      with whatever is already in .env.production so re-running is safe.
#   3. Writes .env.production (mode 0600).
#   4. Brings the stack up: docker compose -f docker-compose.prod.yml
#      --profile production up -d.
#   5. Runs database migrations against the freshly-started Postgres.
#   6. Records the first platform admin's KChat user id in
#      KAPP_PLATFORM_ADMIN_USERS so they are promoted to platform admin on
#      their first SSO login (see internal/auth/sso.go::bootstrapAdmin).
#
# Re-running the script preserves already-generated secrets and existing
# answers, so it doubles as a "reconcile my .env.production and restart"
# command. Nothing here is destructive.
#
# Requirements: bash, docker (with the compose plugin), openssl.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
COMPOSE_FILE="$REPO_ROOT/docker-compose.prod.yml"
ENV_FILE="$REPO_ROOT/.env.production"

# Deterministic project name so the migration helper can address the
# compose-created network (<project>_default) regardless of CWD.
PROJECT_NAME="kapp"
export COMPOSE_PROJECT_NAME="$PROJECT_NAME"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarn:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

require() {
    command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

require docker
require openssl
if ! docker compose version >/dev/null 2>&1; then
    die "the docker compose plugin is required (got legacy docker-compose or none)"
fi
[ -f "$COMPOSE_FILE" ] || die "compose file not found: $COMPOSE_FILE"

# gen_secret prints a URL-safe random secret of a fixed, guaranteed
# length: 32 random bytes rendered as 64 hex chars. hex is used instead
# of base64 so there are no '+' / '/' / '=' chars to strip — which means
# the length is exact (no risk of `cut` silently producing a short
# secret). 64 chars is well above the 32-byte minimum the auth layer
# enforces for the JWT secret and the field-level encryption master key
# (tenant.loadKeyFromEnv accepts it via either its base64 or raw path).
gen_secret() {
    openssl rand -hex 32
}

# Load any existing answers so re-runs preserve generated secrets and
# pre-fill prompts. The file is trusted operator-authored config.
if [ -f "$ENV_FILE" ]; then
    log "found existing $ENV_FILE — preserving secrets, pre-filling prompts"
    set -a
    # shellcheck source=/dev/null
    . "$ENV_FILE"
    set +a
fi

# ensure_secret VAR — set VAR to a fresh secret only if it is empty/unset.
ensure_secret() {
    local name="$1" current="${!1:-}"
    if [ -z "$current" ]; then
        printf -v "$name" '%s' "$(gen_secret)"
        log "generated $name"
    else
        log "$name already set — keeping existing value"
    fi
}

# prompt VAR "Question" "fallback-default" — read a value, defaulting to
# the current env value, then the supplied fallback.
prompt() {
    local name="$1" question="$2" fallback="${3:-}"
    local current="${!1:-}" def reply
    def="${current:-$fallback}"
    if [ -n "$def" ]; then
        read -r -p "$question [$def]: " reply || true
        reply="${reply:-$def}"
    else
        read -r -p "$question: " reply || true
    fi
    printf -v "$name" '%s' "$reply"
}

log "Generating / preserving secrets"
ensure_secret KAPP_JWT_SECRET
ensure_secret KAPP_MASTER_KEY
ensure_secret POSTGRES_PASSWORD
ensure_secret APP_DB_PASSWORD
ensure_secret ADMIN_DB_PASSWORD
ensure_secret ZK_FABRIC_ADMIN_TOKEN
ensure_secret ZK_FABRIC_ACCESS_KEY
ensure_secret ZK_FABRIC_SECRET_KEY

# Stripe placeholder keys — overwritten with real keys when billing is
# wired up, but generated as recognisable test placeholders so the app
# boots and the billing module is exercisable end to end.
: "${STRIPE_SECRET_KEY:=sk_test_$(gen_secret)}"
: "${STRIPE_WEBHOOK_SECRET:=whsec_$(gen_secret)}"

log "Deployment configuration"
prompt KAPP_DOMAIN  "Public domain for automatic HTTPS (Caddy)" "localhost"
prompt ACME_EMAIL   "Email for Let's Encrypt expiry notices"    ""
prompt KCHAT_BASE_URL "KChat base URL (SSO)"                     ""
prompt KCHAT_API_KEY  "KChat API key"                           ""

# First platform admin: the operator's KChat user id. It is recorded in
# KAPP_PLATFORM_ADMIN_USERS (comma-separated) and the user is promoted to
# platform admin on first SSO login.
prompt FIRST_ADMIN_KCHAT_ID "First platform admin KChat user id" ""
KAPP_PLATFORM_ADMIN_USERS="${KAPP_PLATFORM_ADMIN_USERS:-}"
if [ -n "$FIRST_ADMIN_KCHAT_ID" ]; then
    case ",${KAPP_PLATFORM_ADMIN_USERS}," in
        *",${FIRST_ADMIN_KCHAT_ID},"*)
            log "admin $FIRST_ADMIN_KCHAT_ID already listed" ;;
        *)
            if [ -n "$KAPP_PLATFORM_ADMIN_USERS" ]; then
                KAPP_PLATFORM_ADMIN_USERS="${KAPP_PLATFORM_ADMIN_USERS},${FIRST_ADMIN_KCHAT_ID}"
            else
                KAPP_PLATFORM_ADMIN_USERS="$FIRST_ADMIN_KCHAT_ID"
            fi ;;
    esac
fi

log "Writing $ENV_FILE"
umask 077
cat > "$ENV_FILE" <<EOF
# Generated by scripts/docker-prod-setup.sh — DO NOT commit.
# Re-run the script to reconcile values; secrets below are preserved.
KAPP_ENV=production

# --- Database -------------------------------------------------------------
POSTGRES_DB=${POSTGRES_DB:-kapp}
POSTGRES_USER=${POSTGRES_USER:-kapp}
POSTGRES_PASSWORD=${POSTGRES_PASSWORD}
APP_DB_USER=${APP_DB_USER:-kapp_app}
APP_DB_PASSWORD=${APP_DB_PASSWORD}
ADMIN_DB_USER=${ADMIN_DB_USER:-kapp_admin}
ADMIN_DB_PASSWORD=${ADMIN_DB_PASSWORD}

# --- Auth / crypto secrets ------------------------------------------------
KAPP_JWT_SECRET=${KAPP_JWT_SECRET}
KAPP_MASTER_KEY=${KAPP_MASTER_KEY}
KAPP_PLATFORM_ADMIN_USERS=${KAPP_PLATFORM_ADMIN_USERS}

# --- KChat SSO ------------------------------------------------------------
KCHAT_BASE_URL=${KCHAT_BASE_URL}
KCHAT_API_KEY=${KCHAT_API_KEY}

# --- Billing (Stripe) -----------------------------------------------------
STRIPE_SECRET_KEY=${STRIPE_SECRET_KEY}
STRIPE_WEBHOOK_SECRET=${STRIPE_WEBHOOK_SECRET}

# --- ZK Object Fabric -----------------------------------------------------
ZK_FABRIC_ENDPOINT=http://zk-fabric:8080
ZK_FABRIC_ADMIN_TOKEN=${ZK_FABRIC_ADMIN_TOKEN}
ZK_FABRIC_ACCESS_KEY=${ZK_FABRIC_ACCESS_KEY}
ZK_FABRIC_SECRET_KEY=${ZK_FABRIC_SECRET_KEY}
ZK_FABRIC_BACKUP_BUCKET=${ZK_FABRIC_BACKUP_BUCKET:-kapp-backups}

# --- Reverse proxy / TLS --------------------------------------------------
KAPP_DOMAIN=${KAPP_DOMAIN}
ACME_EMAIL=${ACME_EMAIL}

# --- Backups --------------------------------------------------------------
BACKUP_CRON=${BACKUP_CRON:-0 3 * * *}
EOF
chmod 600 "$ENV_FILE"

log "Starting the production stack"
docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" --profile production up -d

log "Waiting for Postgres to accept connections"
for _ in $(seq 1 30); do
    if docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" \
        exec -T postgres pg_isready -U "${POSTGRES_USER:-kapp}" -d "${POSTGRES_DB:-kapp}" >/dev/null 2>&1; then
        break
    fi
    sleep 2
done

log "Running database migrations"
# cmd/migrate needs the Go toolchain and the migrations/ tree. Run it in a
# throwaway golang container attached to the compose network so it can
# reach the postgres service by name without publishing the DB port.
MIGRATE_DB_URL="postgres://${POSTGRES_USER:-kapp}:${POSTGRES_PASSWORD}@postgres:5432/${POSTGRES_DB:-kapp}?sslmode=disable"
# Pin the migration toolchain to the exact Go version go.mod declares so the
# throwaway build never drifts from the version the service binaries are built
# with; fall back to the minor tag if go.mod cannot be read.
GO_VERSION="$(awk '/^go [0-9]/ {print $2; exit}' "$REPO_ROOT/go.mod")"
docker run --rm \
    --network "${PROJECT_NAME}_default" \
    -v "$REPO_ROOT":/src \
    -w /src \
    -e DB_URL="$MIGRATE_DB_URL" \
    "golang:${GO_VERSION:-1.25}" \
    go run ./cmd/migrate up

if [ -n "$FIRST_ADMIN_KCHAT_ID" ]; then
    log "First platform admin recorded: $FIRST_ADMIN_KCHAT_ID"
    log "They will be promoted to platform admin on their first SSO login."
else
    warn "No first platform admin configured. Set KAPP_PLATFORM_ADMIN_USERS"
    warn "in $ENV_FILE and re-run, or promote a user via the admin API."
fi

log "Done. Stack is up; check status with:"
printf '    docker compose --env-file %s -f %s --profile production ps\n' "$ENV_FILE" "$COMPOSE_FILE"
