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

# Sort numerically by leading prefix so duplicates land adjacent in
# the array and we can scan a single pass instead of an O(n^2)
# map lookup.
IFS=$'\n' files=($(printf '%s\n' "${files[@]}" | sort))
unset IFS

prev_num=-1
fail=0
declare -A seen

for f in "${files[@]}"; do
    base="$(basename "$f")"
    if [[ ! "$base" =~ ^([0-9]{6})_[^.]+\.sql$ ]]; then
        echo "::error file=$f::migration filename must match '<6-digit-prefix>_<slug>.sql' (got: $base)"
        fail=1
        continue
    fi
    prefix="${BASH_REMATCH[1]}"
    # strip leading zeros for arithmetic comparison; ${prefix#0...}
    # keeps a literal "000000" as "0" rather than empty string.
    num=$((10#$prefix))

    if [[ -n "${seen[$prefix]:-}" ]]; then
        echo "::error file=$f::duplicate migration prefix $prefix (already used by ${seen[$prefix]})"
        fail=1
        continue
    fi
    seen[$prefix]=$base

    if [[ $prev_num -lt 0 ]]; then
        # First file in the sorted sequence must be 000001.
        if [[ $num -ne 1 ]]; then
            echo "::error file=$f::sequence must start at 000001 (got: $prefix)"
            fail=1
        fi
    elif [[ $num -le $prev_num ]]; then
        # Strictly increasing. Exact duplicates are already caught by
        # the $seen map above; this is a defensive guard. Gaps (num >
        # prev_num + 1) are intentionally ALLOWED: migration prefixes
        # are coordinated across parallel workstreams and a number is
        # sometimes reserved on another branch before it lands on main,
        # producing a temporary, benign gap that golang-migrate applies
        # without issue. See scripts comment block and migratesource's
        # Validate() for the rationale.
        echo "::error file=$f::sequence must be strictly increasing; got $prefix after $(printf '%06d' "$prev_num")"
        fail=1
    fi
    prev_num=$num
done

if [[ $fail -ne 0 ]]; then
    echo "migration-numbering-check: ${#seen[@]} files inspected, sequence rejected"
    exit 1
fi

echo "migration-numbering-check: ${#seen[@]} files inspected, sequence well-formed (000001 → $(printf '%06d' "$prev_num"))"
