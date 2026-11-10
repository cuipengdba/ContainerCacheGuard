#!/usr/bin/env bash
# Optional agents never determine the vendor business exit status.
set +e
BASE=/opt/guards
mkdir -p /tmp/container-guards/log /tmp/container-guards/crg /tmp/container-guards/ccg
chmod 700 /tmp/container-guards /tmp/container-guards/{log,crg,ccg}
business= helpers= stopping=0
stop_all() {
  (( stopping )) && return
  stopping=1
  trap - TERM INT
  [[ -z "$business" ]] || kill -TERM "$business" 2>/dev/null
  for p in $helpers; do kill -TERM "$p" 2>/dev/null; done
  [[ -z "$business" ]] || wait "$business" 2>/dev/null
}
trap 'stop_all; exit 143' TERM
trap 'stop_all; exit 130' INT
"$@" & business=$!
agent_loop() {
  local name=$1 bin=$2 cfg=$3 child= nap= delay=10
  stop_agent() {
    trap - TERM INT
    [[ -z "$nap" ]] || kill -TERM "$nap" 2>/dev/null
    if [[ -n "$child" ]]; then
      kill -TERM "$child" 2>/dev/null
      for ((i=0;i<20;i++)); do kill -0 "$child" 2>/dev/null || break; sleep .1; done
      kill -KILL "$child" 2>/dev/null
    fi
    exit 0
  }
  trap stop_agent TERM INT
  while :; do
    if [[ -x "$bin" && -r "$cfg" ]]; then
      # Bounded per-agent output; each restart rotates only its own files.
      [[ ! -f "/tmp/container-guards/log/$name.log" ]] || mv -f "/tmp/container-guards/log/$name.log" "/tmp/container-guards/log/$name.previous.log"
      "$bin" run --config "$cfg" >> "/tmp/container-guards/log/$name.log" 2>&1 & child=$!
      wait "$child"; child=
    fi
    sleep "$delay" & nap=$!; wait "$nap"; nap=
    (( delay < 300 )) && delay=$((delay*2))
    (( delay > 300 )) && delay=300
  done
}
agent_loop crg "$BASE/bin/crg" "$BASE/config/crg.json" & helpers="$helpers $!"
agent_loop ccg "$BASE/bin/containercacheguard" "$BASE/config/ccg.json" & helpers="$helpers $!"
while kill -0 "$business" 2>/dev/null; do sleep 1 & wait $!; done
wait "$business"; result=$?; business=
stop_all
exit "$result"
