# woodhouse-tasmota

A Woodhouse bridge for [Tasmota](https://tasmota.github.io/docs/)-flashed devices. It polls each
configured device over its HTTP API and exposes it to Woodhouse as a relay device, forwarding
on/off actions back to the device.

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
