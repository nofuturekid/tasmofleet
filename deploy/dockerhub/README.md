# TasmoFleet

A self-hosted fleet manager for [Tasmota](https://tasmota.github.io/) devices: configuration
backups with restore, a settings policy with compliance view, safe bulk firmware updates, live
state and fleet-wide commands. One Go binary with an embedded SQLite database.

**Source, docs and issues: [github.com/nofuturekid/tasmofleet](https://github.com/nofuturekid/tasmofleet)**
· [Try the demo](https://nofuturekid.github.io/tasmofleet/) (the real UI with a simulated fleet)

TasmoFleet is an independent project, not affiliated with Tasmota or its maintainers.

## What it does

- **Backups with diff and restore**: the raw `.dmp` plus decoded JSON
  ([decode-config](https://github.com/tasmota/decode-config) is bundled in the image), scheduled
  and before every update, compared side by side, restored onto the same or a replacement device.
- **Policy and compliance**: rules such as `PowerOnState 0` or `TelePeriod 300` per chip family,
  tag or device, a view of deviating devices and "make compliant".
- **Firmware updates run on the server** as persisted jobs with bounded parallelism, behind a
  safety guard (chip family checked against the image header, no safeboot images, no silent
  variant change). 1 MB ESP8266 devices go through `tasmota-minimal` first.
- **Live state over MQTT** (optional); without MQTT everything works over HTTP polling.
- **Sign-in** with OIDC, trusted forward-auth headers or local accounts; roles and an audit log.
- **Import from TasmoAdmin** (`devices.csv`, `MyConfig.json`).

Nothing is changed on a device unless someone starts an action in the UI.

## Quick start

```sh
docker run -d --name tasmofleet \
  -p 8443:8080 -p 80:80 \
  -e TZ=Europe/Berlin \
  -v tasmofleet-data:/data \
  nofuturekid/tasmofleet:latest
docker logs tasmofleet 2>&1 | grep -o '/setup/account?token=[^"]*'
```

Open `https://<host>:8443` followed by the printed `/setup/account?token=...` path, accept the
self-signed certificate and follow the setup.

- **8080** (container): the UI. Map it to any host port.
- **80** (container): firmware for the devices over plain HTTP; any port the devices can reach.
- **/data**: database, backups and firmware cache. The container runs as UID 65532; for a bind
  mount, `chown -R 65532:65532` the directory first.

Docker Compose, Unraid and all settings:
[Quick start](https://github.com/nofuturekid/tasmofleet#quick-start-docker) and
[configuration](https://github.com/nofuturekid/tasmofleet/blob/main/docs/configuration.md).

## Tags

- `latest`: the newest release
- `X.Y.Z`: a release ([changelog](https://github.com/nofuturekid/tasmofleet/blob/main/CHANGELOG.md))
- `edge`: the current `main`, not for production

The same images are on `ghcr.io/nofuturekid/tasmofleet`. linux/amd64 only.

## License

[AGPL-3.0-or-later](https://github.com/nofuturekid/tasmofleet/blob/main/LICENSE)
