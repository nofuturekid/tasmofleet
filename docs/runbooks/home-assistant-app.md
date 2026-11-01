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
and Core images.

1. Clone `https://github.com/home-assistant/apps-example` and open it with the Dev Containers extension
   (or `devcontainer up`).
2. Build the image on the host and load it into the devcontainer's Docker:
   `docker build --target ha --build-arg VERSION=dev -t ghcr.io/nofuturekid/tasmofleet-ha:dev .`, then
   `docker save ghcr.io/nofuturekid/tasmofleet-ha:dev | docker exec -i <devcontainer> docker load`.
3. Copy `deploy/homeassistant/tasmofleet/` into the devcontainer's `/addons/tasmofleet` (the local app
   folder). The repository's `version: "dev"` matches the tag above.
4. Run the task **Start Home Assistant**, open `http://localhost:7123`, finish onboarding, enable
   advanced mode, open Settings, Apps, the three-dot menu, **Check for updates**; TasmoFleet is under
   **Local apps**.
5. Install and start it. Check on the Info tab: the port table shows `8080` and `8081` with their
   descriptions, **Open Web UI** opens `https://<host>:8080`, the configuration tab offers
   _Initial admin password_, the Log tab is plain text and shows the setup link.

Exit checks of phase 1 (record the result of each in the PR or issue):

- Start `cmd/tasmofleet-devenv` on the host (it serves simulated Tasmota devices), add one in TasmoFleet.
- Set the firmware base URL to `http://<host>:8081`, run an update through the firmware port.
- Restart the app and update it (bump `version` and load the new image); data is still there.
- Make an app backup in Home Assistant, uninstall the app, restore: the devices, the accounts and a
  stored device password (open a device) are back.

If the Supervisor ignores a locally loaded image for a local app and tries to pull, comment out `image:`
in the copied `config.yaml` and add a `Dockerfile` next to it that copies a prebuilt binary.
