#!/usr/bin/env bash
# PID 1 command for a backup-only container. CCG is restarted independently,
# so a CCG fault does not abort an active pg_basebackup in this container.
set -Eeuo pipefail
: "${CCG_BIN:=/opt/ccg/containercacheguard}"
: "${CCG_CONFIG:=/etc/ccg/output.json}"

guard_pid=
stopping=0
shutdown() {
  if (( stopping )); then return; fi
  stopping=1
  trap - TERM INT
  [[ -z "$guard_pid" ]] || kill -TERM "$guard_pid" 2>/dev/null || true
  [[ -z "$guard_pid" ]] || wait "$guard_pid" 2>/dev/null || true
}
trap 'shutdown; exit 0' TERM INT
start_guard() {
  if "$CCG_BIN" doctor -config "$CCG_CONFIG" >/dev/null; then
    "$CCG_BIN" run -config "$CCG_CONFIG" &
    guard_pid=$!
  else
    echo 'CCG configuration check failed; backup worker stays up' >&2
    guard_pid=
  fi
}
start_guard
while :; do
  if [[ -z "$guard_pid" ]] || ! kill -0 "$guard_pid" 2>/dev/null; then
    if [[ -n "$guard_pid" ]]; then
      set +e
      wait "$guard_pid"
      status=$?
      set -e
      echo "CCG exited with status $status; retrying after 15 seconds" >&2
    fi
    sleep 15 &
    wait $! || true
    (( stopping )) && exit 0
    start_guard
  fi
  sleep 5 &
  wait $! || true
done
