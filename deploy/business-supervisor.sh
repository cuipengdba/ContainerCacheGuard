#!/usr/bin/env bash
# Keep the vendor DBS entrypoint intact while restarting CCG independently.
# A CCG crash must never force a PostgreSQL/DBS Pod restart.
set -Eeuo pipefail
: "${CCG_BIN:=/opt/ccg/containercacheguard}"
: "${CCG_CONFIG:=/etc/ccg/source.json}"
: "${BUSINESS_START:=/root/start.sh}"

business_pid=
guard_pid=
stopping=0

stop_both() {
  if (( stopping )); then return; fi
  stopping=1
  trap - TERM INT
  [[ -z "$business_pid" ]] || kill -TERM "$business_pid" 2>/dev/null || true
  [[ -z "$guard_pid" ]] || kill -TERM "$guard_pid" 2>/dev/null || true
  [[ -z "$business_pid" ]] || wait "$business_pid" 2>/dev/null || true
  [[ -z "$guard_pid" ]] || wait "$guard_pid" 2>/dev/null || true
}
trap 'stop_both; exit 0' TERM INT

start_guard() {
  if "$CCG_BIN" doctor -config "$CCG_CONFIG" >/dev/null; then
    "$CCG_BIN" run -config "$CCG_CONFIG" &
    guard_pid=$!
  else
    echo 'CCG configuration check failed; DBS continues, retrying after 15 seconds' >&2
    guard_pid=
  fi
}

"$BUSINESS_START" &
business_pid=$!
start_guard

while :; do
  if ! kill -0 "$business_pid" 2>/dev/null; then
    set +e
    wait "$business_pid"
    status=$?
    set -e
    stop_both
    exit "$status"
  fi
  if [[ -z "$guard_pid" ]] || ! kill -0 "$guard_pid" 2>/dev/null; then
    if [[ -n "$guard_pid" ]]; then
      set +e
      wait "$guard_pid"
      status=$?
      set -e
      echo "CCG exited with status $status; DBS continues, retrying after 15 seconds" >&2
    fi
    sleep 15 &
    wait $! || true
    (( stopping )) && exit 0
    start_guard
  fi
  sleep 3 &
  wait $! || true
done
