#!/usr/bin/env bash
# check_migration_numbering.sh — enforce that every SQL migration in
# the supplied directory has a unique, zero-padded, strictly monotonic
# numeric prefix starting at 000001 with no gaps and no duplicates.
#
# Usage:  check_migration_numbering.sh <migrations-dir>
#
# Exits 0 when the sequence is well-formed, 1 otherwise. Emits
# GitHub-Actions-flavoured `::error::` lines so the CI run links the
# specific offending filename in the PR's "Files changed" tab.
#
# Phase 5 of the security hardening introduced this guard after a
# rebase regression let two migrations share the prefix 000046 while
# 000030 went unassigned. The duplicate was harmless under the
# current psql-glob apply machinery but became a hard error once
# the project considered adopting golang-migrate (which keys on
# unique versions). The check is also useful for new contributors
# who number a migration with `wc -l` or `find -newest` — the
# resulting collision now surfaces at PR time rather than at deploy.

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

    if [[ $prev_num -ge 0 ]]; then
        expected=$((prev_num + 1))
        if [[ $num -ne $expected ]]; then
            printf -v expected_padded "%06d" "$expected"
            echo "::error file=$f::non-monotonic sequence; expected prefix ${expected_padded} after $(printf '%06d' "$prev_num") but got $prefix"
            fail=1
        fi
    elif [[ $num -ne 1 ]]; then
        echo "::error file=$f::sequence must start at 000001 (got: $prefix)"
        fail=1
    fi
    prev_num=$num
done

if [[ $fail -ne 0 ]]; then
    echo "migration-numbering-check: ${#seen[@]} files inspected, sequence rejected"
    exit 1
fi

echo "migration-numbering-check: ${#seen[@]} files inspected, sequence well-formed (000001 → $(printf '%06d' "$prev_num"))"
