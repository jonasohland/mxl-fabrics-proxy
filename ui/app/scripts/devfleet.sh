#!/usr/bin/env bash
#
# The fake fleet of docs/ui.md §9: five nodes with areas, inventory and labels, registered through
# the agent API with their leases kept alive, plus the two namespaces the read-only live suites
# assume — `nab` (exclusive) and `k8s` (shared). No MXL, libfabric, worker or hardware.
#
#   npm run devfleet                      # builds and starts a throwaway server on :12999
#   S=http://127.0.0.1:12999 npm run devfleet   # attaches to one already running there
#
# If nothing answers at $S, a server is built from this tree and started on a fresh sqlite store in
# a temp directory, which is removed again on exit — so every run starts from an empty store rather
# than from whatever an earlier run left behind. If something does answer, the fleet is registered
# into it and the namespaces are re-applied; every write here is create-or-update, so that is safe
# to repeat.
#
# It keeps running on purpose. Leases need renewing, and a lease that expires freezes every path
# touching that node. A heartbeat answered with `reregister` (a restarted server, a wiped store)
# registers the node again and re-posts its inventory, so the fleet survives the server underneath
# it being restarted without being restarted itself.
#
# Paths reach ESTABLISHING and stop, because nothing runs a worker. That is the useful fixture: it
# is the state an operator watches while something comes up.

set -euo pipefail

S=${S:-http://127.0.0.1:12999}
S=${S%/}

# 2283 is what a real mxl-replicator listens on. This script writes node registrations through the
# agent API, which are durable and have no deregister API — pointed at somebody's fleet, it would
# leave five fake nodes in their store for good.
case "$S" in
  *:2283 | *:2283/*)
    if [[ ${DEVFLEET_ALLOW_2283:-} != 1 ]]; then
      echo "devfleet: refusing $S — 2283 is a real mxl-replicator's port." >&2
      echo "devfleet: use 12999, or set DEVFLEET_ALLOW_2283=1 if this really is a throwaway server." >&2
      exit 1
    fi
    ;;
esac

for tool in curl jq; do
  command -v "$tool" >/dev/null || { echo "devfleet: needs $tool on PATH" >&2; exit 1; }
done

REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)

log() { printf 'devfleet: %s\n' "$*" >&2; }

# ---------------------------------------------------------------------------------------------------
# The server, if there is not one already
# ---------------------------------------------------------------------------------------------------

# Leases, the server binary, its store and its log. Gone on exit.
WORK=$(mktemp -d "${TMPDIR:-/tmp}/devfleet.XXXXXX")
SERVER_PID=

cleanup() {
  trap - EXIT INT TERM
  if [[ -n $SERVER_PID ]]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

ready() { curl -fsS -o /dev/null --max-time 1 "$S/readyz"; }

if ready; then
  log "attaching to the control plane at $S"
else
  case "$S" in
    http://127.0.0.1:* | http://localhost:*) ;;
    *) log "nothing answers at $S, and it is not a local address to start one on"; exit 1 ;;
  esac
  listen=${S#http://}
  listen=${listen/localhost/127.0.0.1}

  log "building mxl-replicator"
  (cd "$REPO" && go build -o "$WORK/mxl-replicator" ./cmd/mxl-replicator)

  log "starting a server on $listen, store in $WORK (log: $WORK/server.log)"
  "$WORK/mxl-replicator" run --server \
    --server-listen "$listen" \
    --server-store-sqlite-path "$WORK/store.db" \
    --server-heartbeat-interval 1s --server-lease-ttl 8s \
    >"$WORK/server.log" 2>&1 &
  SERVER_PID=$!

  for _ in $(seq 100); do
    ready && break
    kill -0 "$SERVER_PID" 2>/dev/null || { cat "$WORK/server.log" >&2; exit 1; }
    sleep 0.1
  done
  ready || { log "server did not become ready"; cat "$WORK/server.log" >&2; exit 1; }
fi

# ---------------------------------------------------------------------------------------------------
# HTTP
# ---------------------------------------------------------------------------------------------------

# post PATH BODY — prints the response body; fails loudly on anything but 2xx.
post() {
  local out status
  out=$(curl -sS -w '\n%{http_code}' -XPOST -H 'Content-Type: application/json' \
    --data-binary "$2" "$S$1") || return 1
  status=${out##*$'\n'}
  out=${out%$'\n'*}
  if [[ $status != 2* ]]; then
    log "POST $1 → $status $out"
    return 1
  fi
  printf '%s' "$out"
}

# ---------------------------------------------------------------------------------------------------
# The fleet
# ---------------------------------------------------------------------------------------------------

NODES=(studio-a studio-b edge-01 edge-02 archive-01)

# area NAME READ WRITE
area() { jq -nc --arg n "$1" --argjson r "$2" --argjson w "$3" \
  '{name: $n, path: ("/dev/shm/mxl/" + $n), read: $r, write: $w}'; }

areas() {
  case "$1" in
    studio-a | studio-b) jq -sc . <(area media true false) ;;
    # Both ends of a path: `media` is read-only here and still has an observed domain in it.
    edge-01) jq -sc . <(area media true false) <(area fast true true) <(area bulk true true) ;;
    edge-02) jq -sc . <(area fast true true) ;;
    archive-01) jq -sc . <(area bulk true true) ;;
  esac
}

registration() {
  jq -nc --arg node "$1" --argjson areas "$(areas "$1")" '{
    node: $node,
    instance: ("devfleet-" + $node),
    capabilities: {
      fabrics: [{
        provider: "tcp", fabric: "devfleet", address: "127.0.0.1",
        caps_flags: ["REMOTE_WRITE", "SEND_RECEIVE"], max_message_size: 1048576
      }],
      versions: {protocol: 1, replicator: "devfleet"},
      sched_prio: false,
      areas: $areas
    }
  }'
}

# flow ID GROUP TYPE LABEL PRODUCING [REPLICATED]
#
# GROUP empty means no group hint at all. The definition is NMOS-shaped enough for the UI's summary
# (label, format, geometry, rate) and carries the grouphint tag the agent would have parsed.
flow() {
  jq -nc --arg id "$1" --arg group "$2" --arg type "$3" --arg label "$4" \
    --argjson producing "$5" --argjson replicated "${6:-false}" '
    ($type | if . == "audio" then
        {format: "urn:x-nmos:format:audio", media_type: "audio/float32",
         sample_rate: {numerator: 48000}, grain_rate: {numerator: 48000, denominator: 1}}
      else
        {format: "urn:x-nmos:format:video", media_type: "video/v210",
         frame_width: 1920, frame_height: 1080, grain_rate: {numerator: 50, denominator: 1}}
      end) as $media
    | {
        id: $id,
        flow_def: ({id: $id, label: $label, description: $label}
          + $media
          + {tags: (if $group == "" then {} else
                    {"urn:x-nmos:tag:grouphint/v1.0": [$group + ":" + $type]} end)}),
        producing: $producing
      }
    + (if $group == "" then {} else {group_hint: {name: $group, type: $type}} end)
    + (if $replicated then {replicated: true} else {} end)'
}

# domain AREA ELEMENT FLOW...
domain() {
  local area=$1 element=$2
  shift 2
  jq -nc --arg a "$area" --arg e "$element" --argjson flows "$(printf '%s\n' "$@" | jq -sc .)" \
    '{domain: {area: $a, elements: [$e]}, flows: $flows}'
}

# The flow IDs are fixed because the suites name some of them: 5592a23b… is the one `k8s/cam1-pin`
# pins and `Detail.live.ts` opens, and Unrouted.live.ts looks for 6d3f2a91… and 2f9c6b18….
domains() {
  case "$1" in
    studio-a)
      # Camera 1 is video *and* audio, so a group hint with no type matches two flows; Camera 2 is
      # video only; one flow carries no hint at all, and nobody is producing into it — so `k8s/wall`
      # has a PAUSED path. Talkback is idle too: routing it is how `nab/talkback` is PAUSED.
      domain media cameras \
        "$(flow 5592a23b-0974-45bb-9388-89ea81c42537 'Studio A:Camera 1' video 'Camera 1' true)" \
        "$(flow a1c4e7f2-3b58-4d9a-8e21-6f0b9c3d7a45 'Studio A:Camera 1' audio 'Camera 1' true)" \
        "$(flow b7e2d913-6c4a-4f85-9a3e-2d1c8b7f6e09 'Studio A:Camera 2' video 'Camera 2' true)" \
        "$(flow c93f5a28-1d7e-4b64-a0c9-7e4f2b8d1a36 '' video 'Clean feed' false)"
      domain media audio \
        "$(flow d4a81c6e-9f23-4e7b-b5d0-3a6e1f9c8b72 'Studio A:Talkback' audio 'Talkback' false)"
      ;;
    studio-b)
      # The same shape with its own Camera 1, so a fan-in over both studios is one intent with two
      # sources. No Camera 2: `staged/third` selects it on purpose to match nothing.
      domain media cameras \
        "$(flow e6b03d7a-4c19-4a2f-8d6e-9b5a1f3c2e80 'Studio B:Camera 1' video 'Camera 1' true)"
      ;;
    edge-01)
      # A domain on the node that is also a destination: one flow produced here, which nothing in
      # the fixture routes, and one that replication wrote here.
      domain media local \
        "$(flow 6d3f2a91-8e47-4c05-b1a9-5f2d7c3e8b14 'Edge 01:Monitor' video 'Monitor' true)" \
        "$(flow 2f9c6b18-5a3d-4e71-9c28-0b7e4d1f6a93 'Edge 01:Onward' video 'Onward' true true)"
      ;;
    *) ;;
  esac
}

inventory() {
  jq -nc --arg node "$1" --argjson domains "$(domains "$1" | jq -sc .)" \
    '{node: $node, instance: ("devfleet-" + $node), domains: $domains}'
}

LEASES=$WORK/leases
mkdir "$LEASES"

register() {
  local node=$1 response
  response=$(post /agent/v1/register "$(registration "$node")") || return 1
  jq -r .lease <<<"$response" >"$LEASES/$node"
  post "/agent/v1/$node/inventory" "$(inventory "$node")" >/dev/null
}

# heartbeat NODE — renews the lease; returns 2 if the node had to be registered again.
heartbeat() {
  local node=$1 lease body out status
  lease=$(<"$LEASES/$node")
  body=$(jq -nc --arg n "$node" --arg l "$lease" '{node: $n, instance: ("devfleet-" + $n), lease: $l}')
  out=$(curl -sS -w '\n%{http_code}' -XPOST -H 'Content-Type: application/json' \
    --data-binary "$body" "$S/agent/v1/$node/heartbeat" 2>/dev/null) || return 1
  status=${out##*$'\n'}
  out=${out%$'\n'*}
  # Two ways of being told to start over: a 200 saying so (the lease expired) and a 409 `reregister`
  # (the store holds no lease for this node at all — a restarted server, a wiped store).
  if [[ $status == 409 ]] || jq -e '.reregister == true' <<<"$out" >/dev/null 2>&1; then
    log "$node: re-registering"
    register "$node" && return 2
  elif [[ $status != 2* ]]; then
    log "$node: heartbeat → $status $out"
  fi
}

# ---------------------------------------------------------------------------------------------------
# Labels, and the namespaces the read-only suites assume
# ---------------------------------------------------------------------------------------------------

# label NODE AREA ELEMENT KEY=VALUE...
#
# An apply rather than a patch: it owns exactly these keys, so re-running it changes nothing and a
# key another client (Labels.live.ts's `zone`) wrote is left alone.
label() {
  local node=$1 area=$2 element=$3
  shift 3
  local labels
  labels=$(printf '%s\n' "$@" | jq -Rn '[inputs | split("=") | {(.[0]): (.[1:] | join("="))}] | add')
  post "/v1/nodes/$node/domains" "$(jq -nc --arg a "$area" --arg e "$element" --argjson l "$labels" \
    '{domain: {area: $a, elements: [$e]}, apply: $l}')" >/dev/null
}

namespace() { post /v1/namespaces "$1" >/dev/null; }
request() { post "/v1/namespaces/$1/requests" "$2" >/dev/null; }

seed() {
  label studio-a media cameras role=cameras name=cameras studio=a
  label studio-a media audio role=audio name=audio studio=a
  label studio-b media cameras role=cameras name=cameras studio=b
  label edge-01 media local role=onward name=local

  # `nab`, exclusive: exactly Matrix.live.ts's FIXTURE, which rewrites it on every run anyway.
  # Health, Detail and Ledger read it without seeding it, so it has to exist before any of them run.
  namespace '{"name":"nab","paths":"exclusive"}'
  request nab '{"name":"wall",
    "sources":[
      {"node":"studio-a","domain":{"name":{"area":"media","elements":["cameras"]}},
       "select":{"group_hint":{"name":"Studio A:Camera 1"}}},
      {"node":"studio-b","domain":{"labels":{"role":"cameras"}},
       "select":{"group_hint":{"name":"Studio B:Camera 1"}}}],
    "destinations":[
      {"node":"edge-01","domain":{"area":"fast","elements":["wall"]}},
      {"node":"edge-02","domain":{"area":"fast","elements":["wall"]}}]}'
  request nab '{"name":"talkback",
    "sources":[{"node":"studio-a","domain":{"name":{"area":"media","elements":["audio"]}},"select":{"all":true}}],
    "destinations":[{"node":"edge-02","domain":{"area":"fast","elements":["ingest"]}}]}'
  request nab '{"name":"archive",
    "sources":[{"node":"studio-a","domain":{"name":{"area":"media","elements":["cameras"]}},
      "select":{"group_hint":{"name":"Studio A:Camera 2"}}}],
    "destinations":[{"node":"archive-01","domain":{"area":"bulk","elements":["capture"]},"disabled":true}]}'
  request nab '{"name":"future",
    "sources":[{"node":"studio-a","domain":{"name":{"area":"media","elements":["cameras"]}},
      "select":{"group_hint":{"name":"Studio A:Camera 9"}}}],
    "destinations":[{"node":"edge-01","domain":{"area":"fast","elements":["wall"]}}]}'

  # `k8s`, shared: the fixture §7c is written against (docs/ui.md §9). `wall` takes the whole domain
  # and `cam1-pin` pins one flow inside it, so one path has two claims.
  namespace '{"name":"k8s","paths":"shared","description":"one request per pod"}'
  request k8s '{"name":"wall",
    "sources":[{"node":"studio-a","domain":{"name":{"area":"media","elements":["cameras"]}},"select":{"all":true}}],
    "destinations":[{"node":"edge-01","domain":{"area":"fast","elements":["ingest"]}}]}'
  request k8s '{"name":"cam1-pin",
    "sources":[{"node":"studio-a","domain":{"name":{"area":"media","elements":["cameras"]}},
      "select":{"flow":"5592a23b-0974-45bb-9388-89ea81c42537"}}],
    "destinations":[{"node":"edge-01","domain":{"area":"fast","elements":["ingest"]}}]}'
  request k8s '{"name":"pod-abc12",
    "sources":[{"node":"studio-b","domain":{"labels":{"role":"cameras"}},
      "select":{"group_hint":{"name":"Studio B:Camera 1"}}}],
    "destinations":[
      {"node":"edge-01","domain":{"area":"fast","elements":["ingest"]}},
      {"node":"edge-02","domain":{"area":"fast","elements":["ingest"]}}]}'
  request k8s '{"name":"pod-def34",
    "sources":[{"node":"studio-a","domain":{"name":{"area":"media","elements":["cameras"]}},
      "select":{"group_hint":{"name":"Studio A:Camera 2"}}}],
    "destinations":[{"node":"archive-01","domain":{"area":"bulk","elements":["capture"]},"disabled":true}]}'

  log "labelled the source domains, applied namespaces nab (exclusive) and k8s (shared)"
}

# ---------------------------------------------------------------------------------------------------
# Up, and kept up
# ---------------------------------------------------------------------------------------------------

for node in "${NODES[@]}"; do register "$node"; done
log "registered ${NODES[*]}"
seed
log "fleet is up at $S — Ctrl-C to stop"

# One loop for all five, once a second: comfortably inside the 8s TTL the server is started with, and
# well inside the 15s default of one started elsewhere.
#
# A re-registration means the server lost its leases, and if `k8s` is gone with them the store was
# wiped rather than merely restarted — so the labels and namespaces go back too, and the fleet is as
# usable as it was without anybody restarting this script. Only then: re-applying `nab` over a store
# that still has it would undo whatever the operator was doing to it.
while :; do
  sleep 1
  reregistered=
  for node in "${NODES[@]}"; do
    rc=0
    heartbeat "$node" || rc=$?
    [[ $rc == 2 ]] && reregistered=1
  done
  if [[ -n $reregistered ]] &&
    [[ $(curl -s -o /dev/null -w '%{http_code}' "$S/v1/namespaces/k8s") == 404 ]]; then
    seed || true
  fi
done
