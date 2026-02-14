#!/usr/bin/env bash

##
## Copyright © contributors to CloudNativePG, established as
## CloudNativePG a Series of LF Projects, LLC.
##
## Licensed under the Apache License, Version 2.0 (the "License");
## you may not use this file except in compliance with the License.
## You may obtain a copy of the License at
##
##     http://www.apache.org/licenses/LICENSE-2.0
##
## Unless required by applicable law or agreed to in writing, software
## distributed under the License is distributed on an "AS IS" BASIS,
## WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
## See the License for the specific language governing permissions and
## limitations under the License.
##
## SPDX-License-Identifier: Apache-2.0
##

set -euo pipefail

if [ "${DEBUG-}" = true ]; then
  set -x
fi

ROOT_DIR=$(realpath "$(dirname "$0")/../../")
readonly ROOT_DIR

LOG_FILE=""
REPORT_FILE="${ROOT_DIR}/tests/e2e/out/dynamic_storage_report.json"
POLL_INTERVAL=30
WATCH_PID=""
MAX_FAILURES=10
FAIL_STREAM_LOG="/tmp/dynamic-storage-failures.log"
ENABLE_TAIL=true
RUN_ONCE=false

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
NC='\033[0m'

info()  { echo -e "${CYAN}[INFO]${NC}  $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
fail()  { echo -e "${RED}[FAIL]${NC}  $*" >&2; }

usage() {
  cat <<EOF
Usage:
  $0 --log-file <path> [options]

Options:
  --log-file <path>        E2E log file to monitor (required)
  --report-file <path>     Ginkgo JSON report file (default: ${REPORT_FILE})
  --interval <seconds>     Poll interval for summary output (default: ${POLL_INTERVAL})
  --pid <pid>              Exit automatically when this PID exits
  --max-failures <n>       Max failed test names to print per poll (default: ${MAX_FAILURES})
  --fail-stream-log <path> Output file for streamed failure lines (default: ${FAIL_STREAM_LOG})
  --no-tail                Disable live failure-line tail stream
  --once                   Print one summary snapshot and exit
  --help                   Show this help

Example:
  LOG_FILE="/tmp/e2e-run-\$(date +%Y%m%d-%H%M%S).log"
  ./hack/e2e/run-aks-e2e.sh 2>&1 | tee "\$LOG_FILE" &
  E2E_PID=\$!
  ./hack/e2e/monitor-dynamic-storage.sh --log-file "\$LOG_FILE" --pid "\$E2E_PID"
EOF
}

is_positive_int() {
  case "$1" in
    ''|*[!0-9]*|0) return 1 ;;
    *) return 0 ;;
  esac
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --log-file)
      LOG_FILE="${2:-}"
      shift 2
      ;;
    --report-file)
      REPORT_FILE="${2:-}"
      shift 2
      ;;
    --interval)
      POLL_INTERVAL="${2:-}"
      shift 2
      ;;
    --pid)
      WATCH_PID="${2:-}"
      shift 2
      ;;
    --max-failures)
      MAX_FAILURES="${2:-}"
      shift 2
      ;;
    --fail-stream-log)
      FAIL_STREAM_LOG="${2:-}"
      shift 2
      ;;
    --no-tail)
      ENABLE_TAIL=false
      shift
      ;;
    --once)
      RUN_ONCE=true
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      fail "Unknown argument: $1"
      usage
      exit 1
      ;;
  esac
done

if [[ -z "${LOG_FILE}" ]]; then
  fail "--log-file is required"
  usage
  exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
  fail "jq is required"
  exit 1
fi

if ! is_positive_int "${POLL_INTERVAL}"; then
  fail "--interval must be a positive integer"
  exit 1
fi

if [[ -n "${WATCH_PID}" ]] && ! is_positive_int "${WATCH_PID}"; then
  fail "--pid must be a positive integer"
  exit 1
fi

if ! is_positive_int "${MAX_FAILURES}"; then
  fail "--max-failures must be a positive integer"
  exit 1
fi

TAIL_PID=""

cleanup() {
  if [[ -n "${TAIL_PID}" ]]; then
    kill "${TAIL_PID}" >/dev/null 2>&1 || true
  fi
}

start_failure_tail() {
  local fail_pattern
  fail_pattern='(\[FAILED\]|\[PANICKED\]|FAIL:|panic:|timed out)'

  if ! ${ENABLE_TAIL}; then
    return
  fi

  info "Streaming failures from ${LOG_FILE}"
  info "Writing filtered failure stream to ${FAIL_STREAM_LOG}"

  if command -v stdbuf >/dev/null 2>&1; then
    (
      stdbuf -oL tail -F "${LOG_FILE}" 2>/dev/null \
        | stdbuf -oL grep -E "${fail_pattern}" \
        | tee "${FAIL_STREAM_LOG}"
    ) &
  else
    (
      tail -F "${LOG_FILE}" 2>/dev/null \
        | grep -E "${fail_pattern}" \
        | tee "${FAIL_STREAM_LOG}"
    ) &
  fi

  TAIL_PID=$!
  ok "Failure stream started (pid=${TAIL_PID})"
}

print_summary() {
  local ts metrics total passed failed skipped
  ts=$(date -u +"%Y-%m-%d %H:%M:%S UTC")

  if [[ ! -f "${REPORT_FILE}" ]]; then
    warn "[${ts}] report not found yet: ${REPORT_FILE}"
    return
  fi

  if ! metrics=$(jq -c '
      [.[].SpecReports[]] as $specs
      | {
          total: ($specs | length),
          passed: ($specs | map(select(.State == "passed")) | length),
          failed: ($specs | map(select(.State != "passed" and .State != "skipped")) | length),
          skipped: ($specs | map(select(.State == "skipped")) | length)
        }
    ' "${REPORT_FILE}" 2>/dev/null); then
    warn "[${ts}] report exists but is not parseable yet: ${REPORT_FILE}"
    return
  fi

  total=$(jq -r '.total' <<<"${metrics}")
  passed=$(jq -r '.passed' <<<"${metrics}")
  failed=$(jq -r '.failed' <<<"${metrics}")
  skipped=$(jq -r '.skipped' <<<"${metrics}")

  info "[${ts}] total=${total} passed=${passed} failed=${failed} skipped=${skipped}"

  if [[ "${total}" == "0" ]]; then
    warn "[${ts}] report parsed, but no specs recorded yet"
    return
  fi

  if [[ "${failed}" != "0" ]]; then
    echo "  failing tests:"
    jq -r --argjson max "${MAX_FAILURES}" '
      [
        .[].SpecReports[]
        | select(.State != "passed" and .State != "skipped")
        | (
            if (.LeafNodeText // "") != "" then .LeafNodeText
            elif (.FullText // "") != "" then .FullText
            elif (.Failure.Location.FileName // "") != "" then
              (.Failure.Location.FileName + ":" + ((.Failure.Location.LineNumber // 0) | tostring))
            else "<unnamed>"
            end
          ) as $name
        | ($name + " [" + .State + "]")
      ]
      | unique
      | .[0:$max]
      | .[]
      | "  - " + .
    ' "${REPORT_FILE}" 2>/dev/null || true
  fi
}

trap cleanup EXIT INT TERM

info "Monitoring dynamic-storage E2E run"
info "log=${LOG_FILE}"
info "report=${REPORT_FILE}"

if [[ -n "${WATCH_PID}" ]]; then
  info "watching pid=${WATCH_PID}"
fi

start_failure_tail

if ${RUN_ONCE}; then
  print_summary
  exit 0
fi

while true; do
  print_summary

  if [[ -n "${WATCH_PID}" ]] && ! kill -0 "${WATCH_PID}" >/dev/null 2>&1; then
    ok "watched process exited (pid=${WATCH_PID}); printing final summary"
    print_summary
    break
  fi

  sleep "${POLL_INTERVAL}"
done
