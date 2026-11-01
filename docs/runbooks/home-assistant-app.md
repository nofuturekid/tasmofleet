# Home Assistant app: testing it against a Supervisor

Phase 1 of the Home Assistant app (#442, umbrella #209) is the folder `deploy/homeassistant/tasmofleet/`
and the Dockerfile target `ha`. This runbook is how to install it into a Supervisor that is **not** your
real Home Assistant. Never test on the household's instance.

## What CI proves without a Supervisor

`tools/ha-app-smoke.sh <ha-image> [<default-image>]` (job `ha-app` of `image.yml`, on pull requests that touch the
image, the profile or the app folder, and on `main`) first checks the image metadata: the `ha` image runs as root and has
the `io.hass.type/version/arch` labels, the default image stays non-root and has none. It then starts the `ha` image
like the Supervisor does: a fresh root-owned `/data` volume that is not filled from the image, an `options.json`,
`SUPERVISOR_TOKEN` set, no `TASMOFLEET_` variables. It asserts `/readyz` is 200 on the UI port, the log is plain
text, and a malformed `options.json` stops the container with an error naming the file. The job also cross-builds the
arm64 variant and checks its `io.hass.arch=aarch64` label.

```sh
docker build --target ha -t tasmofleet-ha:dev .
tools/ha-app-smoke.sh tasmofleet-ha:dev tasmofleet:dev   # second image: docker build -t tasmofleet:dev .
```

What it cannot prove: that a Supervisor accepts `config.yaml`, shows the ports, `webui` link and options
form, installs, restarts, updates and backs up the app. That needs a Supervisor.

## Install from a local repository in the Home Assistant devcontainer

Needs Docker with privileged containers and about 10 GB of free disk; the first start pulls the Supervisor
images. Run on a throwaway devcontainer, never on the household's Home Assistant. This is the way phase 1
was verified (2026-11-01), without VS Code:

1. Start the devcontainer image with the volumes its `.devcontainer.json` (home-assistant/apps-example)
   uses; without volumes for `/var/lib/docker` and `/var/lib/containerd` the inner Docker cannot start
   containers:
   `docker run -d --name tf-hadev --privileged -v tf-docker:/var/lib/docker -v tf-containerd:/var/lib/containerd -v tf-sup:/mnt/supervisor -v <dir>:/mnt/supervisor/apps/local/ws -e WORKSPACE_DIRECTORY=/mnt/supervisor/apps/local/ws -e SUPERVISOR_CHANNEL=beta --tmpfs /tmp ghcr.io/home-assistant/devcontainer:5-apps`
   where `<dir>` contains a copy of `deploy/homeassistant/tasmofleet/`.
2. `docker exec -u vscode -e WORKSPACE_DIRECTORY=/mnt/supervisor/apps/local/ws tf-hadev bash devcontainer_bootstrap`, then
   `docker exec -d -u vscode ... tf-hadev bash -c 'supervisor_run > /tmp/sup.log 2>&1'` (the task "Start Home Assistant").
   After about a minute `docker exec tf-hadev docker ps` lists `hassio_supervisor` and `homeassistant`.
3. The Supervisor always pulls `image:`, even for a local app, so publish the image to a registry the inner
   Docker can reach: in the devcontainer `docker run -d -p 5000:5000 registry:2`, load the image
   (`docker build --target ha --build-arg VERSION=dev -t x . && docker save x | docker exec -i tf-hadev docker load`),
   tag and push it as `localhost:5000/tasmofleet-ha:dev`, and change `image:` in the copied `config.yaml` to
   `localhost:5000/tasmofleet-ha`. (`version: "dev"` is the tag.)
4. Use the CLI of the `hassio_cli` container instead of the UI:
   `docker exec tf-hadev docker exec hassio_cli ha apps install local_tasmofleet` (run `ha store reload` first),
   `ha apps info local_tasmofleet` (ports, `webui`, schema), `ha apps start`, `ha apps logs`, and from the
   devcontainer `curl -sk https://127.0.0.1:8080/readyz`.
5. Update: bump `version` in the copied `config.yaml`, push the image with the new tag, `ha store reload`,
   `ha apps update local_tasmofleet`. Backup and restore: `ha backups new --app local_tasmofleet`, uninstall,
   install, start, `ha backups restore <slug> --app local_tasmofleet`.
6. Clean up: `docker rm -f -v tf-hadev` and remove the three volumes.

## Scripted run (CI) and the OTA run by hand

`tools/ha-app-devcontainer-test.sh <ha-image>` does steps 1 to 6 above without a person: it starts the devcontainer,
loads the image into a local registry, checks that the Supervisor accepts `config.yaml` (ports, `webui`, schema),
installs and starts the app with the option `initial_admin_password` (the log must show the admin created from it),
checks `/readyz` on the mapped port, and runs the backup, uninstall, install, restore round trip (the `secret.key` of
the first install must come back). Every wait is bounded; the whole script is limited to 25 minutes. The workflow
`.github/workflows/ha-app.yml` runs it nightly and by `workflow_dispatch`, never on pull requests, with no secrets.

An update through the firmware port, done by hand on 2026-11-01 (#442), in the same devcontainer:

1. Run `cmd/tasmofleet-devenv` inside the devcontainer, bound to the address of the Home Assistant network
   (`-bind 172.30.32.1`, the gateway of the `hassio` Docker network) and with
   `-allow-download-host 172.30.32.1`: the simulated devices only download from loopback unless the host is
   named, and without it an update ends in "device did not start the upgrade" without a request ever arriving.
2. Point the firmware sources at the devenv's fake upstream. A real user does not do this; with the real sources
   the devices would be offered what upstream offers. In the app's data directory
   (`/mnt/supervisor/apps/data/local_tasmofleet/` in the devcontainer) write `tasmofleet.env` with
   `TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE=<DEVENV_UPSTREAM_GITHUB_URL>` and restart the app.
3. In the UI: sign in as `admin` (the option's password; the first sign-in asks for a new one), run the setup with the
   device network `172.30.32.0/23`, then System, Firmware & updates, Firmware base URL
   `http://172.30.32.1:8081` (the host address and the mapped host port), save, and restart the app (the page says
   "Restart required").
4. Add the simulated device (`DEVENV_DEVICE ... addr=` of an HTTP-only profile such as `plug-4m-http`), start an
   update to the version `DEVENV_FW_VERSION`. The firmware request appears in the app log with the device host as
   the client, the transaction ends "Updated to 16.1.0" and the device reports the new version.
