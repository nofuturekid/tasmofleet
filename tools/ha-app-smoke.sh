#!/bin/sh
# Container smoke test of the Home Assistant app image (Dockerfile target "ha"), run the way the
# Supervisor runs it: SUPERVISOR_TOKEN set, a root-owned empty /data with the Supervisor's
# options.json, no TASMOFLEET_ variables. No Supervisor is involved (that is the devcontainer run in
# docs/runbooks/home-assistant-app.md); this proves the image and the HA profile together.
#   1. valid options: /readyz answers on the UI port, the log is plain text
#   2. malformed options.json: the container exits non-zero and the log names the file (fail loud)
#   0. image metadata: the ha image runs as root and carries io.hass.type/version/arch; the DEFAULT image
#      (second argument, optional) stays non-root and carries no io.hass.* label
# /data is a fresh, empty, root-owned volume that is NOT filled from the image (volume-nocopy), like the
# Supervisor's /data. Without that, the image's own /data (owned by 65532) would hide a missing USER root.
# Usage: tools/ha-app-smoke.sh <ha-image> [<default-image>]
set -eu
image=${1:?usage: ha-app-smoke.sh <ha-image> [<default-image>]}
default_image=${2:-}
name=tf-ha-smoke-$$
vol=$name-data
cleanup() { docker rm -f -v "$name" >/dev/null 2>&1 || true; docker volume rm -f "$vol" >/dev/null 2>&1 || true; }
trap cleanup EXIT
work=$(mktemp -d)
trap 'cleanup; rm -rf "$work"' EXIT

# start <options.json body>: creates the container with a fresh volume, copies the file in, starts it.
fail() { echo "FAIL: $*" >&2; exit 1; }

user=$(docker image inspect -f '{{.Config.User}}' "$image")
[ "$user" = root ] || fail "the ha image runs as '$user', the Supervisor's /data needs root"
for l in io.hass.type io.hass.version io.hass.arch; do
  v=$(docker image inspect -f "{{index .Config.Labels \"$l\"}}" "$image")
  [ -n "$v" ] || fail "the ha image has no label $l"
  [ "$l" != io.hass.type ] || [ "$v" = app ] || fail "io.hass.type is '$v', want app"
  [ "$l" != io.hass.arch ] || case "$v" in amd64|aarch64) ;; *) fail "io.hass.arch is '$v', want amd64 or aarch64" ;; esac
done
if [ -n "$default_image" ]; then
  user=$(docker image inspect -f '{{.Config.User}}' "$default_image")
  case "$user" in nonroot|65532|65532:65532) ;; *) fail "the default image runs as '$user', want nonroot (65532)" ;; esac
  docker image inspect -f '{{range $k, $v := .Config.Labels}}{{$k}}{{"\n"}}{{end}}' "$default_image" | grep -q '^io\.hass\.' && fail "the default image carries io.hass.* labels"
fi
echo "ok: image metadata (ha image: root, io.hass labels; default image: ${default_image:-not checked})"

start() {
  cleanup
  printf '%s' "$1" > "$work/options.json"
  docker volume create "$vol" >/dev/null
  docker create --name "$name" -e SUPERVISOR_TOKEN=fake-token-for-the-smoke-test \
    --mount "type=volume,src=$vol,dst=/data,volume-nocopy" -p 127.0.0.1::8080 "$image" >/dev/null
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
