# Metrics

`/metrics` serves the Prometheus text format. It is protected by a bearer token when one
is set (`Authorization: Bearer <token>`). Set it on the settings page (Settings, System,
Metrics, "Scrape token"; write-only, applies at once), or fix it with
`TASMOFLEET_METRICS_TOKEN` / `TASMOFLEET_METRICS_TOKEN_FILE`: the variable then wins and the
field is read-only. Without a token only a scraper on this host that did not come through a
proxy is served; the per-device gauges do not change this. The metric contract lives in `internal/metrics/metrics.go`
(`ContractMetrics`); `deploy/grafana/tasmofleet.json` may reference only those names.

## Fleet aggregates (always on)

See the package documentation of `internal/metrics` for the list (`tasmofleet_devices{health}`,
OTA, firmware, MQTT, HTTP, audit, build info).

## Per-device gauges

Exported only while the setting **Device names in metric labels** (`METRICS_DEVICE_LABELS`, System
settings, Metrics) is on. With it off, none of these series exist. They follow the setting live (within
30 seconds, the refresh interval of the gauges).

Common labels: `device` is the inventory name (escaped by the exposition format: `\\`, `\"`,
`\n`), `id` is the inventory id and stays stable when a device is renamed. No address,
hostname or MAC is ever a label. At most one series per device, and per channel or relay.
Label values never come from payload text: relay and channel are the validated numbers 1 to 8 (energy also empty for a single meter); everything else a device or broker publisher sends is dropped, so the number of series per device is bounded (relay numbers 1 to 8, energy channels none or 1 to 8). Only inventory devices are exported. A device that leaves the inventory disappears at the next refresh.

| Metric                                          | Extra label | Meaning                                                                       |
| ----------------------------------------------- | ----------- | ----------------------------------------------------------------------------- |
| `tasmofleet_device_up`                          |             | 1 online; 0 stale, offline or credentials rejected. Never observed: no series |
| `tasmofleet_device_uptime_seconds`              |             | `UptimeSec` of the last STATE or Status 11 (a drop means a reboot)            |
| `tasmofleet_device_wifi_rssi_percent`           |             | `Wifi.RSSI`                                                                   |
| `tasmofleet_device_wifi_signal_dbm`             |             | `Wifi.Signal`                                                                 |
| `tasmofleet_device_power_watts`                 | `channel`   | `ENERGY.Power`, online devices only                                           |
| `tasmofleet_device_energy_total_kwh`            | `channel`   | `ENERGY.Total`, online devices only                                           |
| `tasmofleet_device_energy_today_kwh`            | `channel`   | `ENERGY.Today`, online devices only                                           |
| `tasmofleet_device_energy_yesterday_kwh`        | `channel`   | `ENERGY.Yesterday`, online devices only                                       |
| `tasmofleet_device_relay_on`                    | `relay`     | 1 on, 0 off; last known state. A single-relay `POWER` is relay `"1"`          |
| `tasmofleet_device_last_seen_timestamp_seconds` |             | Unix time of the last successful observation (MQTT message or HTTP poll)      |

Notes:

- `channel` is empty for a single meter and `"1"`, `"2"`, ... when Tasmota sends arrays
  (`"Power":[12,40]`). Energy series exist only for devices that sent an `ENERGY` block on
  `tele/<topic>/SENSOR`; plugs without a meter have none. Devices polled over HTTP only
  (no MQTT) have no energy series, because the poll sends `Status 11`, which carries none.
- A silent meter would report its last power forever, so the energy and power series are dropped
  while the device is not online. `relay_on`, `last_seen`, `uptime` and the Wi-Fi gauges stay (they are last-known values).
- **Unknown is omitted**, not exported as 0: a device nobody has heard from since the start
  would otherwise look like an outage. Alert on `absent`/staleness separately if needed.
- A scrape never talks to a device. Values come from the health cache, which MQTT telemetry
  and the existing HTTP poll (`Status 11`) fill; the gauges are refreshed from that cache every
  30 seconds, independent of scrapes.

### PromQL examples

```promql
# Devices that are not online right now
tasmofleet_device_up == 0

# Total power of the fleet and per device
sum(tasmofleet_device_power_watts)
sum by (device) (tasmofleet_device_power_watts)

# Energy used today by one device with two channels
sum by (device) (tasmofleet_device_energy_today_kwh{device="dual-meter"})

# Devices that rebooted in the last hour
resets(tasmofleet_device_uptime_seconds[1h]) > 0

# Silent for more than 15 minutes
time() - tasmofleet_device_last_seen_timestamp_seconds > 900

# Weak Wi-Fi
tasmofleet_device_wifi_signal_dbm < -80
```
