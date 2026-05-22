#!/usr/bin/env bash
# Self-tests for check_migration_numbering.sh. Exercises each
# rejection path against synthetic fixture directories under
# scripts/testdata/ so the negative-path coverage stays in lockstep
# with the script's exit-code contract. Invoked from CI via
# `scripts/check_migration_numbering_test.sh` (no test framework
# dependency — pure bash so it runs in any container).

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/check_migration_numbering.sh"

if [[ ! -x "$script" ]]; then
    echo "fatal: $script not executable" >&2
    exit 2
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

run() {
    local name="$1"; shift
    local want="$1"; shift  # expected exit code: 0 = pass, 1 = reject
    local dir="$tmp/$name"
    mkdir -p "$dir"
    # Each remaining arg is a "<num>_<slug>" filename to materialise.
    for f in "$@"; do
        touch "$dir/${f}.sql"
    done
    set +e
    local out; out="$("$script" "$dir" 2>&1)"
    local got=$?
    set -e
    rm -rf "$dir"
    if [[ $got -eq $want ]]; then
        echo "PASS $name (exit=$got)"
        pass=$((pass + 1))
    else
        echo "FAIL $name (want exit=$want, got exit=$got)"
        echo "       output: $out"
        fail=$((fail + 1))
    fi
}

# Happy paths.
run "single migration starts at 1" 0 "000001_initial"
run "monotonic sequence of three" 0 "000001_a" "000002_b" "000003_c"

# Sequence must start at 1.
run "starts at 5 not 1" 1 "000005_too_high"

# Gap in sequence.
run "gap at 3" 1 "000001_a" "000002_b" "000004_d"

# Duplicate prefix.
run "duplicate prefix" 1 "000001_a" "000001_b" "000002_c"

# Bad filename format.
run "missing prefix" 1 "initial_no_prefix"
run "short prefix" 1 "001_too_short"
run "long prefix" 1 "0000001_too_long_a" "0000002_too_long_b"

# Verifies that .sql extension is required (script ignores other
# extensions by virtue of the glob). An empty dir is a hard error.
run "empty dir" 2 # no files passed

echo
echo "$pass passed, $fail failed"
exit $fail
