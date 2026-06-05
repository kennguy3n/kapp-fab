#!/usr/bin/env bash
# provision-cell.sh — cell provisioning hook for the autoscaler's
# ScriptProvisioner (internal/platform/cell_provisioner.go).
#
# The worker invokes this script when KAPP_AUTOSCALE_PROVISION=true and
# KAPP_AUTOSCALE_PROVISIONER=script. It is the integration point for
# docker-compose / bare-metal fleets: replace the marked TODO sections
# with the commands that actually stand up or tear down a cell in your
# environment (docker compose up, a Terraform apply, an Ansible run, …).
#
# Contract with ScriptProvisioner (stdout must end with one JSON object):
#
#   provision <region> <max_tenants> [provider] [zone]
#       Stand up a new cell. Print a JSON Cell object as the LAST line of
#       stdout, e.g.:
#         {"id":"cell-eu-west-1-ab12cd34","region":"eu-west-1",
#          "endpoint":"https://cell.internal:8443","provider":"docker",
#          "zone":"","max_tenants":1000}
#       Human-readable progress may be printed on earlier lines; the
#       provisioner only parses the final JSON line.
#
#   deprovision <cell_id>
#       Tear down a cell. MUST be idempotent: tearing down an
#       already-absent cell exits 0.
#
#   status <cell_id>
#       Print a JSON status object as the LAST line of stdout, e.g.:
#         {"state":"ready","message":""}
#       state is one of: pending | ready | draining | failed | unknown.
#
# The script is intentionally side-effect-free out of the box (the TODO
# blocks are empty) so that wiring it up does not accidentally mutate
# infrastructure before an operator has filled in the real commands.

set -euo pipefail

# json_escape prints its argument as a JSON-safe string fragment
# (without surrounding quotes), escaping backslashes and double quotes.
# It does NOT escape control characters (newlines, tabs, U+0000–U+001F):
# the values passed here (region/provider/zone/endpoint) come from operator
# configuration and must not contain them. If you extend this template to
# emit dynamic values that could include control characters, escape them
# too (or pipe through `jq -Rs .`), otherwise the JSON the Go-side
# parseCellJSON reads will be malformed.
json_escape() {
    local s=${1-}
    s=${s//\\/\\\\}
    s=${s//\"/\\\"}
    printf '%s' "$s"
}

# slugify reduces a string to lowercase alphanumerics and dashes so it is
# safe to embed in a generated cell id.
slugify() {
    local s
    s=$(printf '%s' "${1-}" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9' '-')
    # Collapse repeated dashes and trim leading/trailing ones.
    s=$(printf '%s' "$s" | tr -s '-')
    s=${s#-}
    s=${s%-}
    if [[ -z "$s" ]]; then
        s="default"
    fi
    printf '%s' "$s"
}

# gen_suffix returns a short random hex suffix for cell-id uniqueness.
gen_suffix() {
    if command -v openssl >/dev/null 2>&1; then
        openssl rand -hex 4
    else
        # /dev/urandom fallback keeps the script dependency-free.
        head -c4 /dev/urandom | od -An -tx1 | tr -d ' \n'
    fi
}

usage() {
    cat >&2 <<'EOF'
usage:
  provision-cell.sh provision <region> <max_tenants> [provider] [zone]
  provision-cell.sh deprovision <cell_id>
  provision-cell.sh status <cell_id>
EOF
    exit 2
}

cmd_provision() {
    local region=${1-}
    local max_tenants=${2-}
    local provider=${3-}
    local zone=${4-}

    if [[ -z "$region" || -z "$max_tenants" ]]; then
        echo "provision: region and max_tenants are required" >&2
        return 2
    fi
    if ! [[ "$max_tenants" =~ ^[0-9]+$ ]]; then
        echo "provision: max_tenants must be an integer, got '$max_tenants'" >&2
        return 2
    fi

    local cell_id
    cell_id="cell-$(slugify "$region")-$(gen_suffix)"
    local endpoint=""

    # --- TODO(operator): stand up the cell here. -----------------------
    # Examples:
    #   docker compose -p "$cell_id" up -d
    #   terraform -chdir=infra/cell apply -auto-approve \
    #       -var "region=$region" -var "cell_id=$cell_id"
    # Set "endpoint" to the address the cell-router should use to reach
    # the new cell once it is up.
    # -------------------------------------------------------------------

    echo "provisioning cell $cell_id in region $region (max_tenants=$max_tenants)" >&2

    printf '{"id":"%s","region":"%s","endpoint":"%s","provider":"%s","zone":"%s","max_tenants":%s}\n' \
        "$(json_escape "$cell_id")" \
        "$(json_escape "$region")" \
        "$(json_escape "$endpoint")" \
        "$(json_escape "$provider")" \
        "$(json_escape "$zone")" \
        "$max_tenants"
}

cmd_deprovision() {
    local cell_id=${1-}
    if [[ -z "$cell_id" ]]; then
        echo "deprovision: cell_id is required" >&2
        return 2
    fi

    # --- TODO(operator): tear the cell down here (idempotently). -------
    # Examples:
    #   docker compose -p "$cell_id" down --volumes || true
    #   terraform -chdir=infra/cell destroy -auto-approve \
    #       -var "cell_id=$cell_id"
    # An already-absent cell MUST still exit 0.
    # -------------------------------------------------------------------

    echo "deprovisioned cell $cell_id" >&2
}

cmd_status() {
    local cell_id=${1-}
    if [[ -z "$cell_id" ]]; then
        echo "status: cell_id is required" >&2
        return 2
    fi

    local state="ready"
    local message=""

    # --- TODO(operator): probe real cell health here. -----------------
    # Examples:
    #   if ! docker compose -p "$cell_id" ps --status running >/dev/null; then
    #       state="failed"; message="containers not running"
    #   fi
    # -------------------------------------------------------------------

    printf '{"state":"%s","message":"%s"}\n' \
        "$(json_escape "$state")" \
        "$(json_escape "$message")"
}

main() {
    local action=${1-}
    if [[ -z "$action" ]]; then
        usage
    fi
    shift

    case "$action" in
        provision)   cmd_provision "$@" ;;
        deprovision) cmd_deprovision "$@" ;;
        status)      cmd_status "$@" ;;
        -h | --help | help) usage ;;
        *)
            echo "unknown action: $action" >&2
            usage
            ;;
    esac
}

main "$@"
