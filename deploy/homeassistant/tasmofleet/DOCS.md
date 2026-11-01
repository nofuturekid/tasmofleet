# TasmoFleet

TasmoFleet manages your Tasmota devices: configuration backups with restore, a settings policy with
a compliance view, safe firmware updates (single devices or in bulk) and fleet-wide commands.

**Status: experimental (phase 1).** This app only opens two ports. There is no sidebar entry
(ingress) yet, no Home Assistant sign-in, and the MQTT broker, the firmware address and the device
networks are not filled in for you. Those come in later phases (#443, #444). Everything below works
today.

## Open the web interface

1. Start the app and open its log. On the first start it prints a one-time setup link (and, without
   the option below, creates the first administrator `admin`).
2. Click **Open Web UI**, or browse to `https://<your Home Assistant host>:8080`. The certificate is
   self-signed, so the browser warns once; accept it.
3. Follow the setup link to set the administrator password, then sign in. TasmoFleet has its own
   accounts; Home Assistant users get no access automatically.

## Ports

| Container port | Default host port | Use                                                                      |
| -------------- | ----------------- | ------------------------------------------------------------------------ |
| `8080/tcp`     | `8080`            | Web interface, HTTPS with a self-signed certificate.                     |
| `80/tcp`       | `8081`            | Firmware server your Tasmota devices download updates from (plain HTTP). |

Change the host ports on the app's **Network** tab if they are taken. The firmware server is the
reason for the second port: Tasmota cannot fetch updates over HTTPS.

## First setup in TasmoFleet

Settings live in TasmoFleet's own database (System settings page), not in this app's configuration.

- **Device networks**: the address ranges your Tasmota devices live in. Nothing is contacted until
  you set them.
- **Firmware base URL**: the address the devices use to reach the firmware server, for example
  `http://192.168.1.10:8081` (the Home Assistant host and the host port above, not `localhost`).
  The firmware server starts once this is set; restart the app if it does not answer afterwards.
- **MQTT** (optional): enter your broker (for example the Mosquitto app) by hand. It is not looked up
  automatically yet.

## Configuration

| Option                   | Use                                                                                             |
| ------------------------ | ----------------------------------------------------------------------------------------------- |
| `initial_admin_password` | Optional. Password of the first administrator on a fresh install; changed at the first sign-in. |

Everything else is set in TasmoFleet itself. The full variable reference is in
[docs/configuration.md](https://github.com/nofuturekid/tasmofleet/blob/main/docs/configuration.md#home-assistant-app).

## Data and backups

All data (database, encryption key, firmware cache, device backups) is in the app's private `/data`
and is part of every Home Assistant backup. The app backup is **cold**: the app is stopped while the
backup runs, so the database is copied consistently. If a firmware update is running at that moment it
is interrupted and picked up again after the restart; TasmoFleet checks the device before it acts and
never repeats the upgrade command blindly. Avoid starting a backup while an update runs.

Restoring a backup restores the encryption key with the database, so stored device passwords keep
working.

## Raspberry Pi (aarch64)

The app runs on aarch64. The backup decoder (decoded JSON view and comparison of backups) is an
unofficial build of the upstream tool there; raw backups and restore work either way.

## Support

Issues and questions: <https://github.com/nofuturekid/tasmofleet/issues>.
