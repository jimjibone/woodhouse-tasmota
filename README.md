# woodhouse-tasmota

A Woodhouse bridge for [Tasmota](https://tasmota.github.io/docs/)-flashed devices. It polls each
configured device over its HTTP API (`Status 0`) and exposes it to Woodhouse, forwarding on/off
and toggle actions back to the device.

## Device shape (schema v2)

Device IDs are `tasmota<mac>` (lowercase, no separators). The `endpoints` file in the store is
still keyed by bare MAC.

- Root endpoint `0`: `info` and `online`. `device_temperature` when the device reports an ESP32
  internal temperature. There is no `update` or `identify`: the bridge cannot do either.
- One endpoint per power channel (`POWER` or `POWER1..N`), ID `1`..`N`, labelled from the
  device's FriendlyName for that channel, else `Output N`. Each has `on_off` with `toggle`.
- `power_meter` from the `ENERGY` sensor object, with only the attributes the device reports
  (voltage, current, power, energy in kWh). One meter on a single-channel device sits on endpoint
  `1`; one meter on a multi-channel device measures the whole device and sits on the root
  endpoint; several meters (array values) map to channels in order.

The endpoints and services are built from the first successful status poll, so a device that is
offline when the bridge starts appears once it first answers. Restart the bridge after changing
the device's channels or sensors.

## Usage

Devices are added explicitly by IP (there's no auto-discovery):

```sh
woodhouse-tasmota --store <path> add-device <device-ip-1> [<device-ip-2> ...]
```

Then run the bridge:

```sh
woodhouse-tasmota --store <path> --addr <woodhouse-server-addr>
```

Key flags:

- `--store` (required) — path to local storage for pairing/config state and the known device list
- `--addr` (required) — Woodhouse server address
- `--id` — bridge ID (default `tasmota`)
- `--debug` / `-v` — enable debug logging

## Taskfile tasks

- `task build` — build the bridge binary into `build-<os>-<arch>/`
- `task run` — build and run the bridge (extra args after `--` are passed through)
- `task init-deploy` — create/populate `.env` with the deploy variables (`DEPLOY_HOST`,
  `DEPLOY_USER`, `DEPLOY_DIR`, `DEPLOY_GOOS`, `DEPLOY_GOARCH`)
- `task deploy` — build for the configured deploy target, rsync it to the remote host, and
  restart its systemd service

`build-component` and `build-and-deploy-component` are internal helpers shared by the tasks
above; they aren't meant to be run directly.
