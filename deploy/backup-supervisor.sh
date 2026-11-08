#!/usr/bin/env bash
# PID 1 command for a backup-only container. A failed CCG causes the Pod to
# restart; the PostgreSQL server and DBS business process are never started.
set -Eeuo pipefail
: "${CCG_BIN:=/opt/ccg/containercacheguard}"
: "${CCG_CONFIG:=/etc/ccg/output.json}"

"$CCG_BIN" doctor -config "$CCG_CONFIG" >/dev/null
"$CCG_BIN" run -config "$CCG_CONFIG" &
guard_pid=$!
shutdown() {
  kill -TERM "$guard_pid" 2>/dev/null || true
  wait "$guard_pid" 2>/dev/null || true
}
trap shutdown TERM INT EXIT
while kill -0 "$guard_pid" 2>/dev/null; do
  sleep 5 &
  wait $! || true
done
wait "$guard_pid"
