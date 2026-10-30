# Configuration reference

Every environment variable of tasmofleet is listed here, once. The table is checked against
`internal/config` by a test (`configdoc_test.go`), so it cannot drift from the code.

A fresh install needs **no** variable at all. Everything that is not a bootstrap setting is
configured on the System settings page after the first sign-in and stored in the database.

## How a variable is read

- Variables carry the prefix `TASMOFLEET_`. The same keys (prefix optional) may be written to
  `/data/tasmofleet.env` as `KEY=value` lines; a real environment variable wins over the file.
  Another file can be named with `TASMOFLEET_CONFIG_FILE`.
- **An empty value counts as not set.** `KEY=` (which is what Unraid passes for an unfilled
  template field) means: use the default and lock nothing.
- Secrets can also be given as `<KEY>_FILE`, the path of a file that holds the value (Docker
  secrets). Setting both the variable and the `_FILE` variant is an error. A file that is empty
  or holds only a newline counts as not set, like an empty variable.
- Every value is trimmed of surrounding white space. Durations use Go syntax (`90s`, `5m`, `2h`).
  Lists are comma separated.

## Kinds

| Kind          | Meaning                                                                                                                                            |
| ------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| **bootstrap** | Environment (or env file) only. Needed before the database exists, or must never be changeable from the web. Shown read-only on the settings page. |
| **lock**      | Optional. Left unset, the setting is edited on the System settings page. Set, the variable wins and the field is read-only.                        |
| **legacy**    | Imported into the database once, at the first start that still sees it, and ignored afterwards. Remove it; the settings page owns the setting.     |

New settings get a page field, not a variable.

## Variables

| Variable                                  | Kind      | Default                        | Secret        | Purpose                                                                                                                                                                                                                 |
| ----------------------------------------- | --------- | ------------------------------ | ------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `TASMOFLEET_DATA_DIR`                     | bootstrap | `/data`                        | no            | Database, `secret.key`, firmware cache, backups, generated certificate. Back it up as a whole.                                                                                                                          |
| `TASMOFLEET_CONFIG_FILE`                  | bootstrap | `<DATA_DIR>/tasmofleet.env`    | no            | The env file to read. A missing default file is ignored; a missing explicit one is an error.                                                                                                                            |
| `TASMOFLEET_LISTEN`                       | bootstrap | `:8080`                        | no            | UI and API listener. One port in every mode; containers do not set it (see [TLS](#how-tls-is-chosen)). For installs outside a container.                                                                                |
| `TASMOFLEET_FIRMWARE_LISTEN`              | bootstrap | `:80`                          | no            | Device-facing firmware server (plain HTTP). Containers do not set it: map the host port instead.                                                                                                                        |
| `TASMOFLEET_PUBLIC_URL`                   | bootstrap |                                | no            | External URL of the UI, e.g. `https://tasmofleet.example.com`. Required for OIDC (redirect and logout target).                                                                                                          |
| `TASMOFLEET_BASE_PATH`                    | bootstrap |                                | no            | Static URL prefix, e.g. `/tasmofleet` (starts with `/`, no trailing `/`).                                                                                                                                               |
| `TASMOFLEET_TRUSTED_PROXIES`              | bootstrap |                                | no            | Comma separated CIDRs of the reverse proxy. Forwarded headers and header sign-in are trusted only from these peers. Also switches the listener to HTTP.                                                                 |
| `TASMOFLEET_TLS_CERT_FILE`                | bootstrap |                                | no            | Certificate for the UI listener (with `TLS_KEY_FILE`, both or neither). Re-read on SIGHUP.                                                                                                                              |
| `TASMOFLEET_TLS_KEY_FILE`                 | bootstrap |                                | no            | Private key for `TLS_CERT_FILE`.                                                                                                                                                                                        |
| `TASMOFLEET_TLS_SANS`                     | bootstrap |                                | no            | Extra DNS names or IPs for the self-signed certificate (localhost, loopback, the hostname and the `PUBLIC_URL` host are always included).                                                                               |
| `TASMOFLEET_ALLOW_INSECURE_HTTP`          | bootstrap | `false`                        | no            | Plain HTTP without a reverse proxy. An explicit opt-in for a trusted LAN.                                                                                                                                               |
| `TASMOFLEET_LOG_FORMAT`                   | bootstrap | `json`                         | no            | `json` or `text`.                                                                                                                                                                                                       |
| `TASMOFLEET_LOG_LEVEL`                    | lock      | `info`                         | no            | `debug`, `info`, `warn` or `error`. Leave unset to change it live on the settings page.                                                                                                                                 |
| `TASMOFLEET_AUTH_MODE`                    | lock      | `local`                        | no            | Primary sign-in: `local`, `oidc` or `header`.                                                                                                                                                                           |
| `TASMOFLEET_OIDC_ISSUER`                  | lock      |                                | no            | OIDC issuer URL. Locks together with the client secret.                                                                                                                                                                 |
| `TASMOFLEET_OIDC_CLIENT_ID`               | lock      |                                | no            | OIDC client id.                                                                                                                                                                                                         |
| `TASMOFLEET_OIDC_CLIENT_SECRET`           | lock      |                                | yes (`_FILE`) | OIDC client secret. Locks together with the issuer.                                                                                                                                                                     |
| `TASMOFLEET_OIDC_SCOPES`                  | lock      | `openid,profile,email`         | no            | Requested scopes (`offline_access` is always added, for the refresh token). Authentik puts `groups` into the profile scope.                                                                                                                                                       |
| `TASMOFLEET_OIDC_GROUPS_CLAIM`            | lock      | `groups`                       | no            | Claim that carries the groups.                                                                                                                                                                                          |
| `TASMOFLEET_OIDC_USERNAME_CLAIM`          | lock      | `preferred_username`           | no            | Claim used as the user name.                                                                                                                                                                                            |
| `TASMOFLEET_OIDC_DISPLAY_NAME`            | lock      | `Single sign-on`               | no            | Login button text: "Sign in with ...".                                                                                                                                                                                  |
| `TASMOFLEET_OIDC_POST_LOGOUT_URL`         | lock      | `PUBLIC_URL` plus base path    | no            | Where the identity provider sends the browser after sign-out.                                                                                                                                                           |
| `TASMOFLEET_ADMIN_GROUPS`                 | lock      |                                | no            | Comma separated groups that map to the admin role.                                                                                                                                                                      |
| `TASMOFLEET_OPERATOR_GROUPS`              | lock      |                                | no            | Comma separated groups that map to the operator role.                                                                                                                                                                   |
| `TASMOFLEET_HEADER_USER`                  | bootstrap | `X-authentik-username`         | no            | Header with the user name (`AUTH_MODE=header`).                                                                                                                                                                         |
| `TASMOFLEET_HEADER_NAME`                  | bootstrap | `X-authentik-name`             | no            | Header with the display name.                                                                                                                                                                                           |
| `TASMOFLEET_HEADER_EMAIL`                 | bootstrap | `X-authentik-email`            | no            | Header with the e-mail address.                                                                                                                                                                                         |
| `TASMOFLEET_HEADER_GROUPS`                | bootstrap | `X-authentik-groups`           | no            | Header with the groups.                                                                                                                                                                                                 |
| `TASMOFLEET_HEADER_GROUPS_SEPARATOR`      | bootstrap | `\|`                           | no            | Separator inside the groups header.                                                                                                                                                                                     |
| `TASMOFLEET_HEADER_LOGOUT_URL`            | bootstrap |                                | no            | Sign-out URL of the forward-auth outpost.                                                                                                                                                                               |
| `TASMOFLEET_LOCAL_LOGIN`                  | bootstrap | `true`                         | no            | Local accounts on the login page next to OIDC or header sign-in (always on in `local` mode). The break-glass access.                                                                                                    |
| `TASMOFLEET_SESSION_IDLE`                 | bootstrap | `2h`                           | no            | Idle time after which a session ends.                                                                                                                                                                                   |
| `TASMOFLEET_SESSION_MAX`                  | bootstrap | `12h`                          | no            | Maximum session age (not below `SESSION_IDLE`).                                                                                                                                                                         |
| `TASMOFLEET_OIDC_RECHECK`                 | bootstrap | `15m`                          | no            | How often the OIDC refresh token and the groups are re-evaluated.                                                                                                                                                       |
| `TASMOFLEET_INITIAL_ADMIN_PASSWORD`       | bootstrap |                                | yes (`_FILE`) | Password of the first local admin on a fresh install (changed at the first sign-in). Empty: a random one is written to `initial-admin-password` in the data directory. Either way a one-time setup link is printed to the log until the admin has a password. |
| `TASMOFLEET_DECODER_PATH`                 | bootstrap | `/usr/local/bin/decode-config` | no            | decode-config binary for decoded backups. Empty disables decoding.                                                                                                                                                      |
| `TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE`   | bootstrap |                                | no            | Replaces all firmware sources with one upstream URL. Tests and development only.                                                                                                                                        |
| `TASMOFLEET_METRICS_TOKEN`                | lock      |                                | yes (`_FILE`) | Bearer token for `/metrics`. Leave unset to set it on the settings page (System, Metrics); a value here locks the field. Without any token only local scrapes are answered. See [metrics](metrics.md).                  |
| `TASMOFLEET_DEVICE_NETWORKS`              | legacy    |                                | no            | CIDRs devices may live in. Set on the settings page; no device is contacted until set.                                                                                                                                  |
| `TASMOFLEET_FIRMWARE_BASE_URL`            | legacy    |                                | no            | URL devices use to fetch firmware, e.g. `http://192.168.1.5:8081`. Required for updates.                                                                                                                                |
| `TASMOFLEET_FIRMWARE_SOURCES`             | legacy    | `github,ota.tasmota.com`       | no            | Firmware sources to cross-check.                                                                                                                                                                                        |
| `TASMOFLEET_FIRMWARE_ALLOW_SINGLE_SOURCE` | legacy    | `false`                        | no            | Accept files only one source offers.                                                                                                                                                                                    |
| `TASMOFLEET_FIRMWARE_REFRESH_INTERVAL`    | legacy    | `6h`                           | no            | Background refresh of the upstream index (`0` = only at start).                                                                                                                                                         |
| `TASMOFLEET_MQTT_URL`                     | legacy    |                                | no            | Broker URL such as `tcp://host:1883`. Empty disables MQTT.                                                                                                                                                              |
| `TASMOFLEET_MQTT_USER`                    | legacy    |                                | no            | MQTT user.                                                                                                                                                                                                              |
| `TASMOFLEET_MQTT_PASSWORD`                | legacy    |                                | yes (`_FILE`) | MQTT password.                                                                                                                                                                                                          |
| `TASMOFLEET_MQTT_FULLTOPIC`               | legacy    | `%prefix%/%topic%/`            | no            | Tasmota FullTopic pattern.                                                                                                                                                                                              |
| `TASMOFLEET_DEVICE_DEFAULT_PASSWORD`      | legacy    |                                | yes (`_FILE`) | The fleet's usual Tasmota web password; prefills the add-device forms.                                                                                                                                                  |
| `TASMOFLEET_TELEPERIOD`                   | legacy    | `300s`                         | no            | TelePeriod expected from devices.                                                                                                                                                                                       |
| `TASMOFLEET_POLL_INTERVAL`                | legacy    | `5m`                           | no            | HTTP poll interval for devices without MQTT.                                                                                                                                                                            |
| `TASMOFLEET_OTA_PARALLEL`                 | legacy    | `2`                            | no            | Devices updated at the same time.                                                                                                                                                                                       |
| `TASMOFLEET_OTA_RESTORE_OTAURL`           | legacy    | `true`                         | no            | Restore the device's OtaUrl after an update.                                                                                                                                                                            |
| `TASMOFLEET_OTA_POWER_MAX_AGE`            | legacy    | `20m`                          | no            | Oldest relay state that may be switched back on after an update.                                                                                                                                                        |
| `TASMOFLEET_BACKUP_KEEP`                  | legacy    | `10`                           | no            | Backups kept per device.                                                                                                                                                                                                |
| `TASMOFLEET_BACKUP_MAX_AGE`               | legacy    | `720h`                         | no            | Backups younger than this are never pruned.                                                                                                                                                                             |
| `TASMOFLEET_BACKUP_INTERVAL`              | legacy    | `0s`                           | no            | Scheduled backups; `0s` = manual only.                                                                                                                                                                                  |
| `TASMOFLEET_DISCOVERY_MQTT`               | legacy    | `true`                         | no            | Listen for Tasmota discovery messages (needs MQTT).                                                                                                                                                                     |
| `TASMOFLEET_DISCOVERY_SCAN`               | legacy    | `false`                        | no            | Scheduled network scan.                                                                                                                                                                                                 |
| `TASMOFLEET_DISCOVERY_SCAN_INTERVAL`      | legacy    | `24h`                          | no            | Scan interval.                                                                                                                                                                                                          |
| `TASMOFLEET_DISCOVERY_SCAN_CIDRS`         | legacy    |                                | no            | Ranges to scan (inside `DEVICE_NETWORKS`, each at most a /22, at most 1024 addresses in all).                                                                                                                                                               |
| `TASMOFLEET_POLICY_LIVE_INTERVAL`         | legacy    | `1h`                           | no            | Background reads for the policy view (`0` = off).                                                                                                                                                                       |
| `TASMOFLEET_POLICY_RULES`                 | legacy    |                                | no            | Policy rules as inline JSON or a file path. Imported once as the managed rules.                                                                                                                                         |
| `TASMOFLEET_METRICS_ENABLED`              | legacy    | `true`                         | no            | Master switch for `/metrics` (off: 404).                                                                                                                                                                                |
| `TASMOFLEET_METRICS_DEVICE_LABELS`        | legacy    | `false`                        | no            | Device names in metric labels.                                                                                                                                                                                          |
| `TASMOFLEET_AUDIT_RETENTION`              | legacy    | `8760h`                        | no            | How long the audit log is kept.                                                                                                                                                                                         |

Not prefixed with `TASMOFLEET_`: `TZ`, see [Time zone](#time-zone).

## How TLS is chosen

The UI listens on **container port 8080 in every mode**. Whether that port speaks TLS or plain
HTTP is decided by the mode, never by the port or a variable:

| Set                                | Result                                                         |
| ---------------------------------- | -------------------------------------------------------------- |
| nothing (a fresh install)          | TLS with a self-signed certificate (kept in `<DATA_DIR>/tls`)  |
| `TRUSTED_PROXIES`                  | plain HTTP, the reverse proxy terminates TLS                   |
| `TLS_CERT_FILE` and `TLS_KEY_FILE` | TLS with that certificate (it wins over trusted proxies)       |
| `ALLOW_INSECURE_HTTP=true`         | plain HTTP without a proxy (explicit opt-in for a trusted LAN) |

Switching between TLS and plain HTTP means adding or removing "Trusted proxies" (or a
certificate). The port mapping never changes. These stay environment settings on purpose: TLS has
to be decided before the first request, before any sign-in.

`TASMOFLEET_LISTEN` is still read, for installs outside a container and for containers saved
with an explicit value (a saved `:8443` keeps working). New container setups leave it out.

## Removed variables

| Variable                                                                                                       | Since | What happens                                                                                                                                                |
| -------------------------------------------------------------------------------------------------------------- | ----- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `TASMOFLEET_TLS_SELF_SIGNED`                                                                                   | 0.17  | Ignored, with a warning at start. The self-signed certificate is the mode when no certificate, proxy or insecure opt-in is configured. Delete the variable. |
| `TASMOFLEET_ADMIN_LISTEN`, `_ADMIN_NETWORKS`, `_ADMIN_TLS_CERT_FILE`, `_ADMIN_TLS_KEY_FILE`, `_ADMIN_TLS_SANS` | 0.4   | Ignored, with a warning at start. The direct admin listener is gone; use a local admin on the login page and `tasmofleet user passwd`.                      |

## Time zone

`TZ` (for example `Europe/Berlin`) sets the container's time zone. It works the same for
`docker run`, Compose and Unraid (Unraid sets it on its own). Without it the container runs in UTC.

- **It changes**: log timestamps, the no-JavaScript fallback text of absolute times, the
  relative texts ("Today 09:41", "Yesterday ...") and plain sentences such as restore banners.
- **It does not change**: absolute times in the UI are shown in the browser's time zone.

```sh
docker run -d --name tasmofleet -p 8443:8080 -p 80:80 -e TZ=Europe/Berlin -v tasmofleet-data:/data ghcr.io/nofuturekid/tasmofleet:latest
```

The same image is also on Docker Hub as `nofuturekid/tasmofleet`.

See also the example env file [deploy/unraid/tasmofleet.env.example](../deploy/unraid/tasmofleet.env.example).
