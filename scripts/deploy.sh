#!/usr/bin/env bash
# deploy.sh — zero-downtime rolling deploy for the Kapp production stack
# (docker-compose.prod.yml, Workstream 4) driven by Workstream 7's
# enhanced migrate CLI.
#
# Flow (each step gates the next; a failure after migrations triggers an
# automated rollback):
#
#   1. migrate pre-check  — refuse the deploy if any pending migration is
#                           NOT backward-compatible (drops, renames,
#                           NOT NULL without DEFAULT). Also yields the
#                           pending count used to bound a rollback.
#   2. migrate apply      — apply pending migrations across all cells,
#                           with a readiness sentinel so the LB drains
#                           in-flight requests, and --canary when more
#                           than one cell is configured.
#   3. rolling restart    — recreate each Go service one replica at a
#                           time behind the load balancer (scale up a new
#                           replica, health-check it, drain an old one).
#   4. health check       — after every replica swap; a failure aborts
#                           the rollout.
#   5. rollback on failure— roll the schema back by the number of
#                           migrations this deploy applied AND restore the
#                           previous image tag.
#   6. isolation audit    — GET /api/v1/admin/isolation-audit to confirm
#                           RLS integrity post-deploy.
#
# The script is idempotent: re-running with the same IMAGE_TAG re-applies
# already-applied migrations as a no-op and re-converges the running
# replicas onto that tag.
#
# Usage:
#   IMAGE_TAG=v0.2.0 scripts/deploy.sh
#   scripts/deploy.sh v0.2.0
#
# Key environment variables (all have sensible defaults except IMAGE_TAG):
#   IMAGE_TAG            REQUIRED. Image tag to roll out (arg 1 also works).
#   ROLLBACK_TAG        Tag to restore on failure. Auto-detected from the
#                       running api container when unset.
#   KAPP_IMAGE          Image repository (default ghcr.io/kennguy3n/kapp-fab).
#   COMPOSE_FILE        Compose file (default docker-compose.prod.yml).
#   ENV_FILE            Env file (default .env.production).
#   KAPP_DOMAIN         Public domain fronted by the Caddy LB (default localhost).
#   DB_URL              Admin DSN for the migrate CLI (built from the env
#                       file's POSTGRES_* values when unset).
#   KAPP_CELL_DSNS      Comma-separated per-cell DSNs (multi-cell canary).
#   KAPP_CELL_HEALTH_URLS  Health URLs index-aligned with KAPP_CELL_DSNS.
#   KAPP_ADMIN_TOKEN    Bearer token for the isolation-audit endpoint.
#   API_REPLICAS / WORKER_REPLICAS / KCHAT_BRIDGE_REPLICAS  Replica counts
#                       (default 2 / 1 / 1). >=2 is required for a truly
#                       gap-free rollout of a given service.
#   READINESS_SENTINEL  Drain file path (default /var/run/kapp/migrating).
#   HEALTH_RETRIES / HEALTH_INTERVAL  Per-replica health poll budget.
#   DRY_RUN=1           Print the docker/curl/migrate commands instead of
#                       running them (control-flow smoke test).
#
# Requirements: bash, docker (compose plugin), curl, and either a local Go
# toolchain or network access to pull the golang image for the migrate CLI.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

COMPOSE_FILE="${COMPOSE_FILE:-$REPO_ROOT/docker-compose.prod.yml}"
ENV_FILE="${ENV_FILE:-$REPO_ROOT/.env.production}"
COMPOSE_PROFILE="${COMPOSE_PROFILE:-production}"
COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-kapp}"
export COMPOSE_PROJECT_NAME

KAPP_IMAGE="${KAPP_IMAGE:-ghcr.io/kennguy3n/kapp-fab}"
KAPP_DOMAIN="${KAPP_DOMAIN:-localhost}"

API_REPLICAS="${API_REPLICAS:-2}"
WORKER_REPLICAS="${WORKER_REPLICAS:-1}"
KCHAT_BRIDGE_REPLICAS="${KCHAT_BRIDGE_REPLICAS:-1}"

READINESS_SENTINEL="${READINESS_SENTINEL:-/var/run/kapp/migrating}"
HEALTH_RETRIES="${HEALTH_RETRIES:-30}"
HEALTH_INTERVAL="${HEALTH_INTERVAL:-3}"
DRY_RUN="${DRY_RUN:-0}"

ISOLATION_AUDIT_URL="${ISOLATION_AUDIT_URL:-https://${KAPP_DOMAIN}/api/v1/admin/isolation-audit}"
KAPP_ADMIN_TOKEN="${KAPP_ADMIN_TOKEN:-}"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarn:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

require() {
	command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

# compose wraps docker compose with the standard flags. In DRY_RUN the
# command is printed instead of executed.
compose() {
	if [ "$DRY_RUN" = "1" ]; then
		printf 'DRY-RUN compose %s\n' "$*"
		return 0
	fi
	docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" --profile "$COMPOSE_PROFILE" "$@"
}

# -------------------------------------------------------------------------
# migrate CLI invocation
# -------------------------------------------------------------------------

# urlencode percent-encodes a string for safe use in a URL userinfo
# component (RFC 3986): unreserved characters pass through, everything
# else becomes %XX. LC_ALL=C makes ${#s}/${s:i:1} iterate raw bytes so
# multibyte characters encode correctly.
urlencode() {
	local s="$1" out="" i c LC_ALL=C
	for ((i = 0; i < ${#s}; i++)); do
		c="${s:i:1}"
		case "$c" in
		[a-zA-Z0-9.~_-]) out+="$c" ;;
		*) out+="$(printf '%%%02X' "'$c")" ;;
		esac
	done
	printf '%s' "$out"
}

# build_db_url derives the admin DSN from the env file when DB_URL is not
# supplied, mirroring docker-prod-setup.sh's connection string. The env
# file is sourced inside a subshell so its variables can never leak into
# the deploy script's scope, and the userinfo is percent-encoded so a
# password containing URL-reserved characters yields a valid DSN.
build_db_url() {
	if [ -n "${DB_URL:-}" ]; then
		printf '%s' "$DB_URL"
		return 0
	fi
	(
		# shellcheck disable=SC1090
		[ -f "$ENV_FILE" ] && . "$ENV_FILE"
		local user="${POSTGRES_USER:-kapp}"
		local pass="${POSTGRES_PASSWORD:-}"
		local db="${POSTGRES_DB:-kapp}"
		local host="${MIGRATE_DB_HOST:-localhost}"
		local port="${MIGRATE_DB_PORT:-5432}"
		printf 'postgres://%s:%s@%s:%s/%s?sslmode=disable' \
			"$(urlencode "$user")" "$(urlencode "$pass")" "$host" "$port" "$db"
	)
}

# migrate runs the Workstream 7 migrate CLI. It prefers a prebuilt binary
# ($MIGRATE_BIN, as produced by CI), then a local Go toolchain, then a
# throwaway golang container attached to the compose network (the same
# pattern docker-prod-setup.sh uses).
migrate() {
	local db_url
	db_url="$(build_db_url)"
	if [ "$DRY_RUN" = "1" ]; then
		printf 'DRY-RUN migrate %s\n' "$*"
		return 0
	fi
	if [ -n "${MIGRATE_BIN:-}" ]; then
		DB_URL="$db_url" \
			KAPP_CELL_DSNS="${KAPP_CELL_DSNS:-}" \
			KAPP_CELL_HEALTH_URLS="${KAPP_CELL_HEALTH_URLS:-}" \
			"$MIGRATE_BIN" "$@"
		return $?
	fi
	if command -v go >/dev/null 2>&1; then
		( cd "$REPO_ROOT" && \
			DB_URL="$db_url" \
			KAPP_CELL_DSNS="${KAPP_CELL_DSNS:-}" \
			KAPP_CELL_HEALTH_URLS="${KAPP_CELL_HEALTH_URLS:-}" \
			go run ./cmd/migrate "$@" )
		return $?
	fi
	local go_version
	go_version="$(awk '/^go [0-9]/ {print $2; exit}' "$REPO_ROOT/go.mod" 2>/dev/null || true)"
	docker run --rm \
		--network "${COMPOSE_PROJECT_NAME}_default" \
		-v "$REPO_ROOT":/src -w /src \
		-e DB_URL="$db_url" \
		-e KAPP_CELL_DSNS="${KAPP_CELL_DSNS:-}" \
		-e KAPP_CELL_HEALTH_URLS="${KAPP_CELL_HEALTH_URLS:-}" \
		"golang:${go_version:-1.25}" \
		go run ./cmd/migrate "$@"
}

# detect_current_version prints the schema version currently applied
# (per `migrate version`), or 0 when the DB has no migrations. It is
# captured BEFORE apply so a failed deploy can roll the schema back to
# exactly this baseline — never below it — regardless of how many
# migrations actually committed across cells.
detect_current_version() {
	[ "$DRY_RUN" = "1" ] && { printf '0'; return 0; }
	local out v
	out="$(migrate version 2>/dev/null || true)"
	v="$(printf '%s\n' "$out" \
		| sed -n 's/.*current version=0*\([0-9][0-9]*\).*/\1/p' | head -n1)"
	printf '%s' "${v:-0}"
}

# -------------------------------------------------------------------------
# health checks
# -------------------------------------------------------------------------

# wait_container_healthy polls a service's /healthz from inside one of its
# running containers, so it works for services not exposed through the LB
# (worker, kchat-bridge) as well as the api.
wait_container_healthy() {
	local svc="$1" port="$2" i cid
	if [ "$DRY_RUN" = "1" ]; then
		printf 'DRY-RUN health %s:%s/healthz\n' "$svc" "$port"
		return 0
	fi
	for ((i = 1; i <= HEALTH_RETRIES; i++)); do
		cid="$(compose ps -q "$svc" 2>/dev/null | head -n1 || true)"
		if [ -n "$cid" ] && docker exec "$cid" \
			wget -q -O /dev/null "http://localhost:${port}/healthz" 2>/dev/null; then
			return 0
		fi
		sleep "$HEALTH_INTERVAL"
	done
	return 1
}

# wait_lb_healthy polls the public health endpoint through the Caddy LB,
# confirming the service is reachable end-to-end after a replica swap.
wait_lb_healthy() {
	local url="$1" i
	if [ "$DRY_RUN" = "1" ]; then
		printf 'DRY-RUN lb-health %s\n' "$url"
		return 0
	fi
	for ((i = 1; i <= HEALTH_RETRIES; i++)); do
		if curl -fsS -o /dev/null --max-time 5 "$url"; then
			return 0
		fi
		sleep "$HEALTH_INTERVAL"
	done
	return 1
}

# -------------------------------------------------------------------------
# rolling restart
# -------------------------------------------------------------------------

# drain_one_old_replica removes a single container of svc that is NOT yet
# running new_ref, returning the service to its desired replica count. If
# none is found (e.g. a same-tag re-deploy) it normalises the scale back
# down instead.
drain_one_old_replica() {
	local svc="$1" new_ref="$2" replicas="$3" cid img victim=""
	if [ "$DRY_RUN" = "1" ]; then
		printf 'DRY-RUN drain old replica of %s\n' "$svc"
		return 0
	fi
	while read -r cid; do
		[ -n "$cid" ] || continue
		img="$(docker inspect -f '{{.Config.Image}}' "$cid" 2>/dev/null || true)"
		if [ "$img" != "$new_ref" ]; then
			victim="$cid"
			break
		fi
	done < <(compose ps -q "$svc")

	if [ -n "$victim" ]; then
		log "  [$svc] draining old replica ${victim:0:12}"
		docker rm -f "$victim" >/dev/null
	else
		compose up -d --no-deps --no-recreate --scale "${svc}=${replicas}" "$svc"
	fi
}

# rolling_restart_service recreates one service one replica at a time:
# scale up by one (the new container picks up the new image because the
# compose config now references IMAGE_TAG), health-check, then drain an
# old replica. Repeats until every replica runs the new image.
rolling_restart_service() {
	local svc="$1" port="$2" replicas="$3"
	local new_ref="${KAPP_IMAGE}:${IMAGE_TAG}"
	local i target
	log "Rolling ${svc} -> ${new_ref} (${replicas} replica(s), one at a time)"
	for ((i = 1; i <= replicas; i++)); do
		target=$((replicas + 1))
		log "  [$svc $i/$replicas] starting a new replica (scale ${svc}=${target})"
		compose up -d --no-deps --no-recreate --scale "${svc}=${target}" "$svc" \
			|| return 1
		if ! wait_container_healthy "$svc" "$port"; then
			warn "[$svc] new replica failed health check"
			return 1
		fi
		drain_one_old_replica "$svc" "$new_ref" "$replicas" || return 1
	done
	# Re-converge to the desired count (drops any straggler) and confirm
	# the service is healthy behind the LB for the api edge.
	compose up -d --no-deps --no-recreate --scale "${svc}=${replicas}" "$svc" || return 1
	return 0
}

# -------------------------------------------------------------------------
# isolation audit
# -------------------------------------------------------------------------

# isolation_audit verifies RLS integrity post-deploy via the admin
# endpoint. A missing token or URL downgrades to a warning rather than a
# hard failure so the script stays usable in environments where the audit
# is run out of band.
isolation_audit() {
	if [ -z "$KAPP_ADMIN_TOKEN" ]; then
		warn "KAPP_ADMIN_TOKEN unset; skipping RLS isolation audit"
		return 0
	fi
	if [ "$DRY_RUN" = "1" ]; then
		printf 'DRY-RUN isolation audit %s\n' "$ISOLATION_AUDIT_URL"
		return 0
	fi
	local resp
	resp="$(curl -fsS --max-time 30 \
		-H "Authorization: Bearer ${KAPP_ADMIN_TOKEN}" \
		"$ISOLATION_AUDIT_URL")" || return 1
	printf '%s\n' "$resp"
	# The handler sets {"passed": true} only when every check passes.
	printf '%s' "$resp" | grep -Eq '"passed"[[:space:]]*:[[:space:]]*true'
}

# -------------------------------------------------------------------------
# rollback
# -------------------------------------------------------------------------

# rollback undoes a failed deploy: roll the schema back to the version
# captured before this run applied any migrations, then restore the
# previous image tag across all services. Targeting the pre-deploy
# version (rather than a fixed step count) keeps a partially-applied
# multi-cell deploy correct: cells still at the baseline are left
# untouched instead of being unwound into unrelated earlier migrations.
rollback() {
	warn "initiating automated rollback"
	if [ "${PENDING:-0}" -gt 0 ]; then
		log "rolling schema back to pre-deploy version ${PRE_VERSION:-0}"
		migrate rollback --to-version "${PRE_VERSION:-0}" \
			|| warn "migration rollback failed; engage on-call (see docs/UPGRADE_RUNBOOK.md)"
	else
		log "no migrations were applied; skipping schema rollback"
	fi

	if [ -n "${ROLLBACK_TAG:-}" ]; then
		log "restoring previous image tag ${ROLLBACK_TAG}"
		IMAGE_TAG="$ROLLBACK_TAG"
		export KAPP_IMAGE_TAG="$ROLLBACK_TAG"
		rolling_restart_service api 8080 "$API_REPLICAS" || warn "api restore imperfect"
		rolling_restart_service worker 9090 "$WORKER_REPLICAS" || warn "worker restore imperfect"
		rolling_restart_service kchat-bridge 8090 "$KCHAT_BRIDGE_REPLICAS" \
			|| warn "kchat-bridge restore imperfect"
	else
		warn "ROLLBACK_TAG unknown; binaries NOT restored — restore manually"
	fi
}

# detect_rollback_tag reads the image tag currently running on the api
# service so a failure can restore it. Best-effort; empty when unknown.
detect_rollback_tag() {
	[ -n "${ROLLBACK_TAG:-}" ] && return 0
	[ "$DRY_RUN" = "1" ] && { ROLLBACK_TAG="previous"; return 0; }
	local cid ref
	cid="$(compose ps -q api 2>/dev/null | head -n1 || true)"
	[ -n "$cid" ] || return 0
	ref="$(docker inspect -f '{{.Config.Image}}' "$cid" 2>/dev/null || true)"
	# ref looks like repo:tag; take everything after the final colon that
	# is not part of a registry:port host segment (tags never contain /).
	case "$ref" in
	*:*) ROLLBACK_TAG="${ref##*:}" ;;
	*) ROLLBACK_TAG="" ;;
	esac
}

# -------------------------------------------------------------------------
# rollout orchestration
# -------------------------------------------------------------------------

# rollout runs the forward deploy and returns non-zero on the first failed
# step. Call it from an `if !` context so set -e is suspended and these
# explicit returns drive the rollback decision.
rollout() {
	local target
	for target in api worker kchat-bridge; do
		case "$target" in
		api) rolling_restart_service api 8080 "$API_REPLICAS" || return 1 ;;
		worker) rolling_restart_service worker 9090 "$WORKER_REPLICAS" || return 1 ;;
		kchat-bridge) rolling_restart_service kchat-bridge 8090 "$KCHAT_BRIDGE_REPLICAS" || return 1 ;;
		esac
	done

	log "Step 4/6: load-balancer health check"
	wait_lb_healthy "https://${KAPP_DOMAIN}/healthz" \
		|| wait_lb_healthy "http://${KAPP_DOMAIN}/healthz" \
		|| { warn "LB health check failed"; return 1; }

	log "Step 6/6: RLS isolation audit"
	isolation_audit || { warn "isolation audit did not pass"; return 1; }
	return 0
}

main() {
	require docker
	if [ "$DRY_RUN" != "1" ]; then
		require curl
		docker compose version >/dev/null 2>&1 \
			|| die "the docker compose plugin is required"
		[ -f "$COMPOSE_FILE" ] || die "compose file not found: $COMPOSE_FILE"
	fi

	IMAGE_TAG="${IMAGE_TAG:-${1:-}}"
	[ -n "$IMAGE_TAG" ] || die "IMAGE_TAG is required (env IMAGE_TAG or first argument)"
	export KAPP_IMAGE_TAG="$IMAGE_TAG"
	export KAPP_IMAGE

	detect_rollback_tag
	log "Deploying ${KAPP_IMAGE}:${IMAGE_TAG} (rollback target: ${ROLLBACK_TAG:-unknown})"

	log "Step 1/6: migration pre-check"
	local precheck_out
	precheck_out="$(migrate pre-check)" \
		|| die "pre-check failed: pending migrations are not backward-compatible"
	printf '%s\n' "$precheck_out"
	PENDING="$(printf '%s\n' "$precheck_out" \
		| sed -n 's/.* \([0-9]\{1,\}\) pending migration.*/\1/p' | head -n1)"
	PENDING="${PENDING:-0}"
	log "pending migrations to apply: ${PENDING}"

	# Snapshot the pre-deploy schema version so a failed apply rolls back
	# to exactly this baseline (see rollback()).
	PRE_VERSION="$(detect_current_version)"
	log "pre-deploy schema version: ${PRE_VERSION}"

	log "Step 2/6: applying migrations (sentinel: ${READINESS_SENTINEL})"
	local apply_args=(apply --readiness-sentinel "$READINESS_SENTINEL")
	# Canary only makes sense with more than one cell configured.
	if [ -n "${KAPP_CELL_DSNS:-}" ] && printf '%s' "${KAPP_CELL_DSNS}" | grep -q ','; then
		apply_args+=(--canary)
	fi
	if ! migrate "${apply_args[@]}"; then
		warn "migration apply failed"
		rollback
		die "deploy aborted during migration apply (rolled back)"
	fi

	log "Step 3/6: rolling restart of Go services"
	if ! rollout; then
		rollback
		die "deploy failed during rollout; rolled back to ${ROLLBACK_TAG:-previous}"
	fi

	log "Deploy of ${KAPP_IMAGE}:${IMAGE_TAG} complete."
}

main "$@"
