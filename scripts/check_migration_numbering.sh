#!/usr/bin/env bash
# check_migration_numbering.sh — enforce that every SQL migration in
# the supplied directory has a unique, zero-padded, strictly increasing
# numeric prefix starting at 000001. Duplicates are rejected; GAPS ARE
# ALLOWED.
#
# Usage:  check_migration_numbering.sh <migrations-dir>
#
# Exits 0 when the sequence is well-formed, 1 otherwise. Emits
# GitHub-Actions-flavoured `::error::` lines so the CI run links the
# specific offending filename in the PR's "Files changed" tab.
#
# Phase 5 of the security hardening introduced this guard after a
# rebase regression let two migrations share the prefix 000046. That
# DUPLICATE is the real hazard — golang-migrate keys on unique versions
# — and it is still rejected here. The check is also useful for new
# contributors who number a migration with `wc -l` or `find -newest` —
# the resulting collision surfaces at PR time rather than at deploy.
#
# Gaps, by contrast, are intentionally tolerated. Migration prefixes are
# assigned across several parallel workstreams; a prefix is sometimes
# reserved by one branch before it lands on main (e.g. 000079 shipping
# while 000078 is still in review elsewhere — see the numbering note in
# migrations/000079_db_maintenance.sql). golang-migrate walks the
# registered versions in order, so a missing prefix is simply "no
# migration at that number" and applies cleanly. The earlier
# no-gaps rule fail-fast `migrate up` (and every DB-backed test) the
# moment such a coordinated gap existed, so it was relaxed.

set -euo pipefail

dir="${1:-migrations}"

if [[ ! -d "$dir" ]]; then
    echo "error: directory $dir does not exist" >&2
    exit 2
fi

shopt -s nullglob
files=("$dir"/*.sql)
shopt -u nullglob

if [[ ${#files[@]} -eq 0 ]]; then
    echo "error: no .sql files in $dir" >&2
    exit 2
fi

fail=0
# Per-version state, keyed by the 6-digit prefix.  A version is one
# logical migration that may be expressed as a single forward-only
# NNNNNN_slug.sql or as an NNNNNN_slug.up.sql / NNNNNN_slug.down.sql
# pair, exactly as the Go source driver groups them.
declare -A seen_slug   # prefix -> slug
declare -A seen_up     # prefix -> filename providing the up direction
declare -A seen_down   # prefix -> filename providing the down direction
declare -A seen_num    # prefix -> numeric value (leading zeros stripped)

# Pass 1: validate each filename and collapse up/down companions into a
# single version.  Order does not matter here, so no pre-sort is needed.
for f in "${files[@]}"; do
    base="$(basename "$f")"
    # Mirror the Go source driver's filenameRE
    # (internal/dbutil/migratesource/legacy.go): a trailing .up.sql or
    # .down.sql is allowed so direction-aware companions can share the
    # directory with legacy forward-only NNNNNN_slug.sql files.  The
    # slug ([^.]+) cannot contain a dot, so it never swallows the
    # optional .up/.down or the .sql suffix.
    if [[ ! "$base" =~ ^([0-9]{6})_([^.]+)(\.(up|down))?\.sql$ ]]; then
        echo "::error file=$f::migration filename must match '<6-digit-prefix>_<slug>[.up|.down].sql' (got: $base)"
        fail=1
        continue
    fi
    prefix="${BASH_REMATCH[1]}"
    slug="${BASH_REMATCH[2]}"
    direction="${BASH_REMATCH[4]}" # "" | "up" | "down"
    # 10#$prefix forces base-10 so a literal "000000" stays "0" rather
    # than being read as octal or collapsing to an empty string.
    num=$((10#$prefix))

    # All companions of a version must share the same slug.
    if [[ -n "${seen_slug[$prefix]:-}" && "${seen_slug[$prefix]}" != "$slug" ]]; then
        echo "::error file=$f::version $prefix has conflicting names '${seen_slug[$prefix]}' and '$slug'"
        fail=1
        continue
    fi
    seen_slug[$prefix]="$slug"
    seen_num[$prefix]=$num

    case "$direction" in
    "" | up)
        if [[ -n "${seen_up[$prefix]:-}" ]]; then
            echo "::error file=$f::version $prefix has duplicate up files (${seen_up[$prefix]} and $base)"
            fail=1
            continue
        fi
        seen_up[$prefix]="$base"
        ;;
    down)
        if [[ -n "${seen_down[$prefix]:-}" ]]; then
            echo "::error file=$f::version $prefix has duplicate down files (${seen_down[$prefix]} and $base)"
            fail=1
            continue
        fi
        seen_down[$prefix]="$base"
        ;;
    esac
done

# Pass 2: walk the unique versions in numeric order.  The 6-digit
# zero-padded prefixes sort lexically in numeric order, so a plain
# `sort` is sufficient.  We enforce: starts at 000001, strictly
# increasing (gaps ALLOWED — see the header comment), and every version
# has an up file (a down-only version is malformed).
prev_num=-1
mapfile -t sorted_prefixes < <(printf '%s\n' "${!seen_num[@]}" | sort)
for prefix in "${sorted_prefixes[@]}"; do
    num=${seen_num[$prefix]}
    if [[ -z "${seen_up[$prefix]:-}" ]]; then
        echo "::error file=$dir/${prefix}_${seen_slug[$prefix]}.down.sql::version $prefix has a down file but no up file"
        fail=1
    fi
    if [[ $prev_num -lt 0 ]]; then
        if [[ $num -ne 1 ]]; then
            echo "::error::sequence must start at 000001 (got: $prefix)"
            fail=1
        fi
    elif [[ $num -le $prev_num ]]; then
        # Strictly increasing rejects duplicate/again-seen versions.
        # Gaps (num > prev_num + 1) are intentionally tolerated.
        echo "::error::sequence must be strictly increasing; got $prefix after $(printf '%06d' "$prev_num")"
        fail=1
    fi
    prev_num=$num
done

if [[ $fail -ne 0 ]]; then
    echo "migration-numbering-check: ${#seen_num[@]} migration(s) inspected, sequence rejected"
    exit 1
fi

echo "migration-numbering-check: ${#seen_num[@]} migration(s) inspected, sequence well-formed (000001 → $(printf '%06d' "$prev_num"))"
