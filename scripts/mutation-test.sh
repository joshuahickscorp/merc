#!/usr/bin/env bash
# Mutation testing for the money and reuse paths.
#
# A passing suite proves the tests ran, not that they would have caught
# anything. This deliberately injects defects into production code and asserts
# the suite FAILS for each one. A mutation that survives is a hole in the tests,
# not a success.
#
#   bash scripts/mutation-test.sh
#
# Every mutation is reverted whether it is caught or not; the tree is restored
# on any exit path.
set -uo pipefail
cd "$(dirname "$0")/.."

CONTROL=control
BACKUP="$(mktemp -d "${TMPDIR:-/tmp}/merc-mutation.XXXXXX")"
cleanup() {
  # Restore every file touched, always.
  if [ -d "$BACKUP" ]; then
    for f in "$BACKUP"/*.bak; do
      [ -e "$f" ] || continue
      base="$(basename "$f" .bak)"
      cp "$f" "$CONTROL/${base//__//}"
    done
    rm -rf "$BACKUP"
  fi
}
trap cleanup EXIT INT TERM

: "${MERC_TEST_DATABASE_URL:?mutation testing needs a database}"

# file|description|sed-expression
MUTATIONS=(
"supplier_accrual.go|accrual adds instead of carrying the remainder|s|carryOut = effective % microUSDPerCent|carryOut = 0|"
"supplier_accrual.go|accrual rounds up instead of flooring cents|s|cashCents = effective / microUSDPerCent|cashCents = (effective + microUSDPerCent - 1) / microUSDPerCent|"
"supplier_accrual.go|supplier accrual lock removed|s| FOR UPDATE||"
"billing_classes.go|reused input counted as physical work|s|ClassPrefixReusedInput: false|ClassPrefixReusedInput: true|"
"billing_classes.go|reused tokens billed at the full rate|s|retained := 1.0 - reuseDiscountShare|retained := 1.0|"
"batch_policy.go|token budget ignored|s|if used+cost > budget {|if false {|"
"prefix_routing.go|prefix warmth ignores its TTL|s|AND last_seen_warm > now() - \$3::interval||"
"exact_reuse.go|non-deterministic requests become cacheable|s|return r.Temperature == 0 \&\& (r.TopP == 0 |return true \&\& (r.TopP == 0 |"
"exact_reuse.go|tenant-scoped references accepted|s|^var tenantScopedRefPattern = regexp.MustCompile(\`\^jobs/\`)|var tenantScopedRefPattern = regexp.MustCompile(\`^ZZZNEVERMATCH\`)|"
)

caught=0
survived=0
declare -a SURVIVORS=()

printf '%-58s %s\n' "mutation" "result"
printf '%-58s %s\n' "--------" "------"

for entry in "${MUTATIONS[@]}"; do
  file="${entry%%|*}"
  rest="${entry#*|}"
  desc="${rest%%|*}"
  expr="${rest#*|}"

  src="$CONTROL/$file"
  [ -f "$src" ] || { printf '%-58s %s\n' "$desc" "SKIP (missing $file)"; continue; }

  cp "$src" "$BACKUP/${file//\//__}.bak"
  sed -i '' "$expr" "$src" 2>/dev/null || sed -i "$expr" "$src" 2>/dev/null

  if ! cmp -s "$src" "$BACKUP/${file//\//__}.bak"; then
    # Build first: a mutation that does not compile is not a useful test.
    if ! (cd "$CONTROL" && go build ./... >/dev/null 2>&1); then
      printf '%-58s %s\n' "$desc" "skip (does not compile)"
    elif (cd "$CONTROL" && go test -count=1 ./... >/dev/null 2>&1); then
      printf '%-58s %s\n' "$desc" "SURVIVED"
      survived=$((survived+1))
      SURVIVORS+=("$desc")
    else
      printf '%-58s %s\n' "$desc" "caught"
      caught=$((caught+1))
    fi
  else
    printf '%-58s %s\n' "$desc" "skip (pattern did not apply)"
  fi

  cp "$BACKUP/${file//\//__}.bak" "$src"
done

echo
echo "mutation-test: $caught caught, $survived survived"
if [ "$survived" -gt 0 ]; then
  echo "surviving mutations are gaps in the suite:"
  for s in "${SURVIVORS[@]}"; do echo "  - $s"; done
  exit 1
fi
exit 0
