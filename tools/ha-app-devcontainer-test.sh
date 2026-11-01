#!/usr/bin/env bash
# Installs the Home Assistant app into a real Supervisor that runs in the official Home Assistant
# devcontainer (a throwaway privileged container with its own Docker; never a household instance),
# starts it, and checks what only a Supervisor can prove (#442, runbook docs/runbooks/home-assistant-app.md):
#   1. the Supervisor accepts deploy/homeassistant/tasmofleet/config.yaml (the store lists the app; after the
#      install ports 8080/8081, webui and the options schema are what config.yaml says)
#   2. install + start with the option initial_admin_password: /readyz is 200 on the mapped port, the
#      option reached the app (the admin is created from it)
#   3. backup, uninstall, install, start (a new secret.key), restore: secret.key is the original again,
#      /readyz is 200
# Every wait is bounded. No secrets: the passwords below are fake and the container is deleted at the end.
# Usage: tools/ha-app-devcontainer-test.sh <image of the Dockerfile target "ha">
set -euo pipefail

image=${1:?usage: ha-app-devcontainer-test.sh <ha image>}
name=${DEVC_NAME:-tf-hadev-ci}
devimage=${DEVC_IMAGE:-ghcr.io/home-assistant/devcontainer:5-apps}
channel=${DEVC_SUPERVISOR_CHANNEL:-beta}
repo=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
data=/mnt/supervisor/apps/data/local_tasmofleet

d() { docker exec "$name" docker "$@"; }
ha() { docker exec "$name" docker exec hassio_cli ha "$@"; }
step() { printf '\n== %s\n' "$*"; }
fail() {
	echo "FAIL: $*" >&2
	dump
	exit 1
}

dump() {
	echo "---- diagnostics (last lines) ----" >&2
	docker exec "$name" docker ps -a --format '{{.Names}} {{.Status}}' 2>&1 | tail -15 >&2 || true
	docker exec "$name" docker logs --tail 40 hassio_supervisor 2>&1 | sed 's/\x1b\[[0-9;]*m//g' | tail -40 >&2 || true
	ha apps logs local_tasmofleet 2>&1 | tail -25 >&2 || true
}

cleanup() {
	docker rm -f -v "$name" >/dev/null 2>&1 || true
	for v in docker containerd sup; do docker volume rm -f "$name-$v" >/dev/null 2>&1 || true; done
	rm -rf "$work"
}
trap cleanup EXIT

# retry <seconds> <command...>: run until it succeeds, at most <seconds>.
retry() {
	local limit=$1 start=$SECONDS
	shift
	until "$@" >/dev/null 2>&1; do
		[ $((SECONDS - start)) -lt "$limit" ] || return 1
		sleep 5
	done
}

readyz() { [ "$(docker exec "$name" curl -sk -o /dev/null -w '%{http_code}' https://127.0.0.1:8080/readyz)" = 200 ]; }
keyhash() { docker exec "$name" sha256sum "$data/secret.key" | cut -d' ' -f1; }
# set the app option through the Supervisor API (the ha CLI has no command for it)
set_option() {
	docker exec "$name" docker exec hassio_cli sh -c "wget -qO- --header \"Authorization: Bearer \$SUPERVISOR_TOKEN\" --header 'Content-Type: application/json' --post-data '{\"options\":{\"initial_admin_password\":\"$1\"}}' http://supervisor/addons/local_tasmofleet/options" >"$work/option.out" && grep -q '"result":"ok"' "$work/option.out"
}

step "workspace: a copy of the app folder whose image is a registry inside the devcontainer"
mkdir -p "$work/ws"
cp -r "$repo/deploy/homeassistant/tasmofleet" "$work/ws/"
# The Supervisor always pulls `image:` (also for a local app), so point it at a local registry.
sed -i 's#^image: .*#image: localhost:5000/tasmofleet-ha#' "$work/ws/tasmofleet/config.yaml"
grep -q '^image: localhost:5000/tasmofleet-ha$' "$work/ws/tasmofleet/config.yaml" || fail "config.yaml has no image line to rewrite"

step "devcontainer $devimage (Supervisor channel $channel)"
docker run -d --name "$name" --privileged \
	-v "$name-docker:/var/lib/docker" -v "$name-containerd:/var/lib/containerd" -v "$name-sup:/mnt/supervisor" \
	-v "$work/ws:/mnt/supervisor/apps/local/ws" \
	-e WORKSPACE_DIRECTORY=/mnt/supervisor/apps/local/ws -e SUPERVISOR_CHANNEL="$channel" \
	--tmpfs /tmp "$devimage" >/dev/null
retry 120 docker exec "$name" docker info || fail "the inner Docker did not start within 120 s"
docker exec -u vscode -e WORKSPACE_DIRECTORY=/mnt/supervisor/apps/local/ws "$name" bash devcontainer_bootstrap >/dev/null 2>&1 || true
docker exec -d -u vscode -e WORKSPACE_DIRECTORY=/mnt/supervisor/apps/local/ws -e SUPERVISOR_CHANNEL="$channel" \
	"$name" bash -c 'timeout 1500 supervisor_run > /tmp/supervisor_run.log 2>&1'
retry 600 ha store reload || fail "the Supervisor did not come up within 600 s"
echo "supervisor: $(d inspect -f '{{.Config.Image}}' hassio_supervisor)"

step "registry with the app image"
docker save "$image" | docker exec -i "$name" docker load >/dev/null
d run -d --name reg -p 5000:5000 registry:2 >/dev/null
retry 120 d inspect -f '{{.State.Running}}' reg || fail "registry did not start"
retry 60 curl -s http://127.0.0.1:5000/v2/ 2>/dev/null || true
d tag "$image" localhost:5000/tasmofleet-ha:dev
retry 120 d push localhost:5000/tasmofleet-ha:dev || fail "could not push the image to the registry"

step "1. the Supervisor accepts config.yaml"
retry 120 ha store reload || fail "store reload"
store=$(ha store apps 2>&1) || fail "ha store apps: $store"
grep -Eq 'slug: local_tasmofleet' <<<"$store" || fail "the store does not list local_tasmofleet"

step "2. install, option, start, /readyz"
retry 300 ha apps install local_tasmofleet || fail "install failed"
# what the Supervisor made of config.yaml (ports, webui, option schema); only an installed app shows it
info=$(ha apps info local_tasmofleet 2>&1) || fail "ha apps info: $info"
for want in "8080/tcp: 8080" "80/tcp: 8081" "webui: https://\[HOST\]:8080" "stage: experimental" "name: initial_admin_password"; do
  echo "$info" | grep -Eq -- "$want" || fail "ha apps info lacks '$want'"
done
echo "$info" | grep -E '^(name|stage|webui|rating|version_latest|ingress|host_network)|8080/tcp|80/tcp|initial_admin_password' | sed 's/^/  /'
set_option "fake-devcontainer-pw-1" || fail "could not set the option"
ha apps start local_tasmofleet >/dev/null || fail "start failed"
retry 120 readyz || fail "/readyz is not 200 on the mapped port 8080"
# (no `cmd | grep -q`: with pipefail, grep closing the pipe early would fail the pipeline)
logs=$(ha apps logs local_tasmofleet 2>&1) || fail "ha apps logs"
grep -q 'created the initial admin account from INITIAL_ADMIN_PASSWORD' <<<"$logs" ||
	fail "the option initial_admin_password did not reach the app"
before=$(keyhash)
echo "secret.key sha256 before: $before"

step "3. backup, uninstall, install, restore"
out=$(ha backups new --app local_tasmofleet --name ci-roundtrip 2>&1) || fail "backup failed: $out"
slug=$(echo "$out" | awk '/^slug:/ {print $2}')
[ -n "$slug" ] || fail "no backup slug in: $out"
ha apps uninstall local_tasmofleet >/dev/null || fail "uninstall failed"
retry 300 ha apps install local_tasmofleet || fail "second install failed"
ha apps start local_tasmofleet >/dev/null || fail "second start failed"
retry 120 readyz || fail "/readyz after reinstall"
fresh=$(keyhash)
[ "$fresh" != "$before" ] || fail "a fresh install must create a new secret.key (the restore would prove nothing)"
ha apps stop local_tasmofleet >/dev/null || true
ha backups restore "$slug" --app local_tasmofleet >/dev/null || fail "restore failed"
retry 120 ha apps info local_tasmofleet || true
ha apps start local_tasmofleet >/dev/null 2>&1 || true
retry 120 readyz || fail "/readyz after restore"
after=$(keyhash)
echo "secret.key sha256 fresh: $fresh"
echo "secret.key sha256 after restore: $after"
[ "$after" = "$before" ] || fail "restore did not bring back the original secret.key"
echo "ok: config accepted, install, option applied, /readyz 200, backup/restore round trip"
