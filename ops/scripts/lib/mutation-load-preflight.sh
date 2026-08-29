#!/usr/bin/env bash
# Load / capacity preflight helpers for the mutation campaign.
#
# Sourced by ops/scripts/mutation-test-parallel.sh and its self-test. Pure decision
# functions take synthetic inputs so the refuse/proceed/waive table can be
# exercised without a live campaign. Nothing here scores a mutant: a refusal is
# an infrastructure precondition, the same class as a setup fault or timeout.
#
# Hard ceiling used only when measuring the clean-source baseline wall time B.
# It is not a suite budget and is not raised to make a red gate green.
MERC_MUTATION_BASELINE_MEASURE_CEILING_SECONDS=600

mutation_read_cpu_count() {
  local cpus
  cpus="$(getconf _NPROCESSORS_ONLN 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 4)"
  if ! [[ "$cpus" =~ ^[1-9][0-9]*$ ]]; then
    cpus=4
  fi
  printf '%s\n' "$cpus"
}

# One-minute load average. Prefer /proc (Linux); fall back to sysctl (macOS).
mutation_read_load1() {
  local load1
  if [ -r /proc/loadavg ]; then
    load1="$(/usr/bin/awk '{ print $1 }' /proc/loadavg)"
  else
    load1="$(sysctl -n vm.loadavg 2>/dev/null | /usr/bin/awk '{ print $2 }')"
  fi
  if ! [[ "$load1" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
    echo "mutation-load-preflight: could not read 1-minute load average" >&2
    return 1
  fi
  printf '%s\n' "$load1"
}

# headroom = max(0, cpus - load1). Printed with two decimals for the report.
mutation_cpu_headroom() {
  local cpus="$1" load1="$2"
  /usr/bin/awk -v cpus="$cpus" -v load1="$load1" 'BEGIN {
    h = cpus - load1
    if (h < 0) h = 0
    printf "%.2f\n", h
  }'
}

mutation_default_min_cpu_headroom() {
  local workers="$1"
  printf '%s\n' "$((workers + 2))"
}

# Free kibibytes on the filesystem that will hold target (df -Pk portable form).
# (Avoid naming the local "path" — zsh treats that as a special array tied to PATH.)
mutation_free_disk_kib() {
  local target="$1"
  # Prefer absolute awk: some constrained agent PATHs omit /usr/bin briefly.
  df -Pk "$target" 2>/dev/null | /usr/bin/awk 'NR == 2 { print $4; exit }'
}

# Postgres reachability for the configured URL, if any. Campaign clusters are
# still local initdb instances; this only answers "is the machine's Postgres
# tooling and optional shared URL alive".
mutation_postgres_reachable() {
  if ! command -v pg_isready >/dev/null 2>&1; then
    printf '%s\n' "unavailable (pg_isready not on PATH)"
    return 1
  fi
  if [ -z "${MERC_TEST_DATABASE_URL:-}" ]; then
    printf '%s\n' "tools-present (no MERC_TEST_DATABASE_URL)"
    return 0
  fi
  if pg_isready -d "$MERC_TEST_DATABASE_URL" >/dev/null 2>&1; then
    printf '%s\n' "reachable"
    return 0
  fi
  printf '%s\n' "unreachable"
  return 1
}

mutation_locale_report() {
  printf 'LC_ALL=%s LANG=%s' "${LC_ALL:-<unset>}" "${LANG:-<unset>}"
}

# Decision over synthetic inputs for self-tests and the live preflight.
# Prints exactly one of: refuse | proceed | waive
mutation_load_precondition_decision() {
  local headroom="$1" threshold="$2" ignore_load="${3:-0}"
  if [ "$ignore_load" = "1" ]; then
    printf '%s\n' "waive"
    return 0
  fi
  if /usr/bin/awk -v h="$headroom" -v t="$threshold" 'BEGIN { exit (h + 0 < t + 0) ? 0 : 1 }'; then
    printf '%s\n' "refuse"
  else
    printf '%s\n' "proceed"
  fi
}

# Suite budget from a just-measured clean-source baseline wall time B (seconds).
# A suite that exceeds 3x its own just-measured baseline is genuinely wrong, not
# merely unlucky under load: max(120s, ceil(3 * B)). B is integer wall seconds
# so 3*B is exact; the 120s floor keeps a trivially fast box from setting a
# budget shorter than the historical constant that already had real headroom.
mutation_derive_suite_timeout_seconds() {
  local baseline_seconds="$1"
  local derived
  if ! [[ "$baseline_seconds" =~ ^[0-9]+$ ]]; then
    echo "mutation-load-preflight: baseline seconds must be a non-negative integer" >&2
    return 2
  fi
  derived=$((3 * baseline_seconds))
  if [ "$derived" -lt 120 ]; then
    derived=120
  fi
  printf '%s\n' "$derived"
}

# Full preflight report + refuse decision for the parallel campaign.
# Sets globals used by the caller:
#   MUTATION_PREFLIGHT_CPU_COUNT
#   MUTATION_PREFLIGHT_LOAD1
#   MUTATION_PREFLIGHT_HEADROOM
#   MUTATION_PREFLIGHT_THRESHOLD
#   MUTATION_PREFLIGHT_FREE_DISK_KIB
#   MUTATION_PREFLIGHT_PG
#   MUTATION_PREFLIGHT_LOCALE
#   MUTATION_PREFLIGHT_DECISION   (refuse|proceed|waive)
#   MUTATION_PREFLIGHT_WAIVED     (0|1)
#
# Args: workers  run_root_parent_path
mutation_run_load_preflight() {
  local workers="$1" run_parent="$2"
  local min_headroom decision free_kib pg_status locale_status

  MUTATION_PREFLIGHT_CPU_COUNT="$(mutation_read_cpu_count)"
  MUTATION_PREFLIGHT_LOAD1="$(mutation_read_load1)" || return 2
  MUTATION_PREFLIGHT_HEADROOM="$(mutation_cpu_headroom "$MUTATION_PREFLIGHT_CPU_COUNT" "$MUTATION_PREFLIGHT_LOAD1")"
  min_headroom="${MERC_MUTATION_MIN_CPU_HEADROOM:-$(mutation_default_min_cpu_headroom "$workers")}"
  if ! [[ "$min_headroom" =~ ^[0-9]+$ ]]; then
    echo "MERC_MUTATION_MIN_CPU_HEADROOM must be a non-negative integer" >&2
    return 2
  fi
  MUTATION_PREFLIGHT_THRESHOLD="$min_headroom"
  free_kib="$(mutation_free_disk_kib "$run_parent")"
  if ! [[ "${free_kib:-}" =~ ^[0-9]+$ ]]; then
    free_kib="unknown"
  fi
  MUTATION_PREFLIGHT_FREE_DISK_KIB="$free_kib"
  pg_status="$(mutation_postgres_reachable)" || true
  MUTATION_PREFLIGHT_PG="$pg_status"
  locale_status="$(mutation_locale_report)"
  MUTATION_PREFLIGHT_LOCALE="$locale_status"

  decision="$(mutation_load_precondition_decision \
    "$MUTATION_PREFLIGHT_HEADROOM" \
    "$MUTATION_PREFLIGHT_THRESHOLD" \
    "${MERC_MUTATION_IGNORE_LOAD:-0}")"
  MUTATION_PREFLIGHT_DECISION="$decision"
  if [ "$decision" = "waive" ]; then
    MUTATION_PREFLIGHT_WAIVED=1
  else
    MUTATION_PREFLIGHT_WAIVED=0
  fi

  cat <<EOF
parallel-mutation-test: load preflight
  cpu_count=${MUTATION_PREFLIGHT_CPU_COUNT}
  load1=${MUTATION_PREFLIGHT_LOAD1}
  cpu_headroom=${MUTATION_PREFLIGHT_HEADROOM}  (cpus - load1, clamped at 0)
  min_cpu_headroom=${MUTATION_PREFLIGHT_THRESHOLD}  (default workers+2=${workers}+2; override MERC_MUTATION_MIN_CPU_HEADROOM)
  free_disk_kib=${MUTATION_PREFLIGHT_FREE_DISK_KIB}  (filesystem holding ${run_parent})
  postgres=${MUTATION_PREFLIGHT_PG}
  locale=${MUTATION_PREFLIGHT_LOCALE}
  decision=${MUTATION_PREFLIGHT_DECISION}
EOF

  if [ "$decision" = "refuse" ]; then
    cat <<EOF >&2
parallel-mutation-test: REFUSING to start — infrastructure precondition, not a mutation result
  measured cpu_headroom=${MUTATION_PREFLIGHT_HEADROOM} is below min_cpu_headroom=${MUTATION_PREFLIGHT_THRESHOLD}
  (cpu_count=${MUTATION_PREFLIGHT_CPU_COUNT} load1=${MUTATION_PREFLIGHT_LOAD1} workers=${workers})
  The campaign needs roughly one core per worker plus the coordinator. Re-run when
  the box is quieter, lower MERC_MUTATION_WORKERS, or set MERC_MUTATION_IGNORE_LOAD=1
  to waive (every artifact will then record load_precondition_waived: true).
EOF
    return 1
  fi
  if [ "$decision" = "waive" ]; then
    echo "parallel-mutation-test: load precondition WAIVED (MERC_MUTATION_IGNORE_LOAD=1); artifacts will record the contention" >&2
  fi
  return 0
}
