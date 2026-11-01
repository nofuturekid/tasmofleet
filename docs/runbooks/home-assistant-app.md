# Home Assistant app: testing it against a Supervisor

Phase 1 of the Home Assistant app (#442, umbrella #209) is the folder `deploy/homeassistant/tasmofleet/`
and the Dockerfile target `ha`. This runbook is how to install it into a Supervisor that is **not** your
real Home Assistant. Never test on the household's instance.

## What CI proves without a Supervisor

`tools/ha-app-smoke.sh <image>` (run by `image.yml` on every pull request that touches the Dockerfile and
on every release) starts the `ha` image like the Supervisor does: root-owned empty `/data`, an
`options.json`, `SUPERVISOR_TOKEN` set, no `TASMOFLEET_` variables. It asserts `/readyz` is 200 on the UI
port, the log is plain text, and a malformed `options.json` stops the container with an error naming the file.

```sh
docker build --target ha -t tasmofleet-ha:dev .
tools/ha-app-smoke.sh tasmofleet-ha:dev
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

Not covered by that run and still to do by hand when needed: a simulated device
(`cmd/tasmofleet-devenv` on the host), an update through the mapped firmware port (set the firmware base URL to
`http://<host>:8081` first; the firmware server only starts once it is set), and the UI in a browser.
