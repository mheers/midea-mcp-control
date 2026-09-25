# midea-control

Local-first, pure-Go tooling for the three Midea Wi-Fi air conditioners on this LAN.

The repository contains:

- a native Go V3 LAN adapter wrapped behind `internal/controller`;
- a native Go client for the Midea SmartHome cloud credential bootstrap;
- credential-safe configuration and LAN discovery;
- a CLI with read, discovery, verification, and explicitly confirmed power commands;
- an MCP server over stdio using the official Go SDK.

Everything is standard-library Go plus pinned pure-Go modules: **no Python, no
`exec`/subprocesses, and no cgo**. It builds with `CGO_ENABLED=0`.

The online research and library comparison is in `docs/research/midea-control-research.md`.

## Current inventory

The three units were discovered on the local subnet and authenticated with their stored LAN credentials:

| Cloud name | IP | Model | Protocol |
|---|---|---|---|
| living-room | `192.0.2.153` | `00000Q1F` | V3 |
| bedroom (bedroom) | `192.0.2.131` | `00000Q18` | V3 |
| kids-room | `192.0.2.130` | `00000Q18` | V3 |

The inventory is stored outside the repository at `~/.config/midea-control/devices.json` with mode `0600`. Tokens and keys are never printed by the CLI or MCP tools.

## Requirements

- Go `1.26` or newer. The Go toolchain can download the required toolchain automatically when `GOTOOLCHAIN=auto` is enabled.
- Nothing else. No Python, no external tools, no cgo.

The native protocol dependency is pinned to the immutable pseudo-version in `go.mod`. It is an MIT-licensed, V3-focused implementation that was tested for discovery, authentication, status reads, and power writes against all three units here, but it is still a hardware-oriented community implementation. The library's own scope is narrower than a general Midea library, so mode/fan/temperature decoding beyond the fields exercised here should be treated as unvalidated. The controller interface is deliberately small so it can be replaced if a better Go implementation appears.

## One-time bootstrap

The Midea account is needed only to fetch each device's long-lived LAN token/key. It is not needed for normal local operation.

```sh
bin/midea-control bootstrap
```

The command prompts for the SmartHome account and password (the password is read
without echo), discovers the units, asks the cloud for a token/key per device,
**verifies each credential against the unit over the LAN**, and only then writes
`~/.config/midea-control/devices.json` with mode `0600`. A unit whose cloud
token does not authenticate is reported and skipped; the others are still
saved. An existing inventory is merged, never replaced blindly, and a file it
cannot parse is left untouched rather than overwritten.

```sh
# Check the cloud handshake without writing anything.
bin/midea-control bootstrap --dry-run

# Re-bootstrap a single re-paired unit.
bin/midea-control bootstrap --device bedroom
```

The password can also be supplied through `MIDEA_PASSWORD` for non-interactive
use, and the account through `MIDEA_ACCOUNT` or `--account`. Neither is ever
written to disk, and neither is reachable through MCP: `bootstrap` is a
CLI-only command by design.

Verify the resulting file permissions before use:

```sh
stat -c '%a %n' ~/.config/midea-control/devices.json
bin/midea-control audit
```

## Build

```sh
GOTOOLCHAIN=auto go build -o bin/midea-control ./cmd/midea-control
```

The default configuration path can be overridden with `MIDEA_CONTROL_CONFIG` or the `--config` flag.

## CLI

```sh
# Configured devices; no credentials are displayed.
bin/midea-control list

# Credential metadata and file permissions, without token/key values.
bin/midea-control audit

# Credential-free UDP discovery.
# The native Go discovery was run on this LAN and found all three units.
bin/midea-control discover --json

# Read all configured units.
bin/midea-control status

# Authenticate every unit using only the saved LAN credentials.
bin/midea-control verify

# Physical writes require an explicit confirmation flag.
bin/midea-control off bedroom --confirm
bin/midea-control on bedroom --confirm

# One-time cloud bootstrap (prompts for the account password).
bin/midea-control bootstrap
```

A selector is an exact configured name, IP address, or numeric device ID. A power write seeds a full state read, changes only the logical power field, and performs an independent read-back. The legacy `0x40` set frame itself is full-state, so firmware can normalize other fields even though the adapter only requests a power change. The upstream client normally retries timed-out commands; this project sets reads to two attempts but physical writes to one, so a set command is never resent automatically. If the command is sent but read-back fails, the tool reports delivery as unknown.

## MCP over stdio

The MCP command opens no network listener. Configure an MCP client with the built binary:

```json
{
  "mcpServers": {
    "midea": {
      "command": "/absolute/path/to/midea-control/bin/midea-control",
      "args": ["mcp", "--config", "/home/marcel/.config/midea-control/devices.json"]
    }
  }
}
```

Available tools:

- `list_devices` — list configured devices without secrets;
- `verify_credentials` — read-only check that authenticates every configured unit with stored LAN credentials and returns an independent result for each unit; the CLI `verify` command records successful `last_verified` metadata;
- `discover_devices` — read-only LAN broadcast discovery;
- `get_status` — read one configured device;
- `set_power` — physical on/off write; requires `confirm: true`, changes only power, and reads back the state.

`confirm` is a guard against accidental calls, not an authorization boundary: an MCP client that is allowed to reach this local server can still set it to `true`. Keep write-capable MCP clients behind your own human-approval policy.

The MCP boundary does not accept raw tokens, keys, or protocol frames. Its optional discovery target is limited to a private IPv4/loopback address and carries only a fixed read-only probe; control operations only accept configured device selectors. If HTTP transport is added later, bind it to loopback and add authentication and Origin validation before considering any non-loopback exposure.

## Token lifetime

There are two different values involved in V3 LAN authentication:

1. The cloud bootstrap returns a 64-byte device token (128 hex characters) and a 32-byte key (64 hex characters). The app-facing response contains no expiry timestamp. Community implementations and the legacy Midea LAN API treat this pair as a long-lived device credential, so this project stores it and re-authenticates each local connection with it.
2. The device handshake derives a temporary per-connection local/TCP key. The `msmart-ng` reference implementation marks that derived key as valid for 12 hours; that is not an expiry on the stored token or key. The Go adapter derives it for each short-lived CLI/MCP operation and does not cache it.

No credential system can guarantee that a vendor-issued device secret never becomes invalid: re-pairing/resetting a unit, changing its Wi-Fi module, or a cloud-side revocation can invalidate it. The practical protections here are:

- keep the mode-`0600` inventory file backed up offline;
- run `midea-control verify` periodically (for example, from a user-level timer) to detect invalidation; a simple hourly crontab entry is `0 * * * * /absolute/path/to/bin/midea-control verify --timeout 30s >/dev/null`;
- rerun `midea-control bootstrap` if a unit is re-paired or a verification fails;
- never commit the inventory or the bootstrap account credentials.

`midea-control audit` deliberately reports `expiry: none reported` rather than claiming an expiry that the vendor did not provide.

## Development

```sh
gofmt -w cmd/midea-control/*.go internal/*/*.go
go test ./...
go vet ./...
CGO_ENABLED=0 go build ./...
```

The tested seams are configuration permissions/validation, LAN discovery
parsing, cloud handshake signing and token retrieval (against a fake cloud
server and vectors captured from the reference implementation), and read/write
command behavior. The write path is tested with a fake protocol client;
physical-device acceptance was verified separately against all three units.
