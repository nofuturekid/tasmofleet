#!/bin/sh
# Container smoke test of the Home Assistant app image (Dockerfile target "ha"), run the way the
# Supervisor runs it: SUPERVISOR_TOKEN set, a root-owned empty /data with the Supervisor's
# options.json, no TASMOFLEET_ variables. No Supervisor is involved (that is the devcontainer run in
# docs/runbooks/home-assistant-app.md); this proves the image and the HA profile together.
#   1. valid options: /readyz answers on the UI port, the log is plain text
#   2. malformed options.json: the container exits non-zero and the log names the file (fail loud)
# Usage: tools/ha-app-smoke.sh <image>
set -eu
image=${1:?usage: ha-app-smoke.sh <image>}
name=tf-ha-smoke-$$
vol=$name-data
cleanup() { docker rm -f -v "$name" >/dev/null 2>&1 || true; docker volume rm -f "$vol" >/dev/null 2>&1 || true; }
trap cleanup EXIT
work=$(mktemp -d)
trap 'cleanup; rm -rf "$work"' EXIT

# start <options.json body>: creates the container with a fresh volume, copies the file in, starts it.
start() {
  cleanup
  printf '%s' "$1" > "$work/options.json"
  docker create --name "$name" -e SUPERVISOR_TOKEN=fake-token-for-the-smoke-test \
    -v "$vol:/data" -p 127.0.0.1::8080 "$image" >/dev/null
  docker cp "$work/options.json" "$name:/data/options.json"
  docker start "$name" >/dev/null
}

start '{"initial_admin_password": "smoke-test-password-1"}'
port=$(docker port "$name" 8080/tcp | head -n1 | sed 's/.*://')
ok=
for _ in $(seq 1 60); do
  if [ "$(curl -sk -o /dev/null -w '%{http_code}' "https://127.0.0.1:$port/readyz" || true)" = 200 ]; then ok=1; break; fi
  [ "$(docker inspect -f '{{.State.Running}}' "$name")" = true ] || break
  sleep 1
done
if [ -z "$ok" ]; then echo "FAIL: /readyz did not answer 200 on the UI port" >&2; docker logs "$name" >&2; exit 1; fi
logs=$(docker logs "$name" 2>&1)
case "$logs" in
  *'"level"'*|'{'*) echo "FAIL: the log is JSON, the HA profile must log plain text" >&2; echo "$logs" >&2; exit 1 ;;
esac
echo "$logs" | grep -q 'tasmofleet starting' || { echo "FAIL: no start line in the log" >&2; echo "$logs" >&2; exit 1; }
echo "ok: valid options, /readyz 200, plain-text log"

start '{not json'
for _ in $(seq 1 30); do
  [ "$(docker inspect -f '{{.State.Running}}' "$name")" = false ] && break
  sleep 1
done
[ "$(docker inspect -f '{{.State.Running}}' "$name")" = false ] || { echo "FAIL: malformed options.json did not stop the container" >&2; exit 1; }
[ "$(docker inspect -f '{{.State.ExitCode}}' "$name")" != 0 ] || { echo "FAIL: exit code 0 on malformed options.json" >&2; exit 1; }
docker logs "$name" 2>&1 | grep -q 'options.json' || { echo "FAIL: the error does not name options.json" >&2; docker logs "$name" >&2; exit 1; }
echo "ok: malformed options.json stops the container, error names the file"
