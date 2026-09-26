# midea-mcp-control

Local-first, pure-Go tooling for Midea Wi-Fi air conditioners on your own LAN.

The repository contains:

- a native Go V3 LAN adapter wrapped behind `internal/controller`;
- a native Go client for the Midea SmartHome cloud credential bootstrap;
- credential-safe configuration and LAN discovery;
- a CLI with read, discovery, verification, and explicitly confirmed writes;
- an MCP server over stdio or authenticated HTTP, using the official Go SDK.

Everything is standard-library Go plus pinned pure-Go modules: **no Python, no
`exec`/subprocesses, and no cgo**. It builds with `CGO_ENABLED=0`.

The library research and protocol notes behind these choices are in
`docs/research/midea-mcp-control-research.md`.

## Status and caveats

- The Midea cloud protocol is reverse-engineered and undocumented. The LAN side
  is stable in practice; the cloud bootstrap endpoints can change without
  notice. If `bootstrap` ever stops working, normal local control will not.
- The LAN protocol dependency is a community project pinned to a single
  immutable commit. It was written for one specific appliance, so decoding
  beyond the fields listed under [Scope](#scope) is unvalidated. It sits behind
  a small internal interface so it can be replaced.
- `sleep`, `eco`, `turbo` and `display` are not honoured by the hardware tested
  here, and `swing` only works while a unit is running. These are properties of
  the firmware, not of this tool.
- Mode and fan-speed *names* come from two independent implementations that
  agree on the wire encoding and read identical values from real units. The
  labels have not been confirmed by observing each mode's physical effect;
  power draw could separate "fan" from compressor modes but not cooling from
  heating.

## Scope

Built and validated against Midea protocol **V3** air conditioners
(`00000Q18` and `00000Q1F` units). It discovers appliances on the local subnet,
obtains their long-lived LAN credentials once, then reads and controls them
without the cloud.

What the write path can change, as measured on real hardware:

| Field | Works | Notes |
|---|---|---|
| power | yes | verified by read-back |
| mode | yes | `auto`, `cool`, `dry`, `heat`, `fan`, `smart_dry` |
| temperature | yes | 17–30 °C in 0.5 °C steps |
| fan speed | yes | `auto`, `silent`, `low`, `medium`, `high`, `full` |
| swing | while running | ignored by a powered-off unit |
| sleep | transient | accepted, then cleared by the unit |
| eco, turbo, display | no | accepted on the wire, refused by the hardware |

Unsupported fields stay exposed and report the disagreement rather than
reporting a false success. See
[what the writes actually do](#what-the-writes-actually-do-on-this-hardware).

The device inventory — including tokens and keys — is stored **outside the
repository** at `~/.config/midea-mcp-control/devices.json` with mode `0600`. No
credential, device identifier, MAC address, or LAN address from a real
deployment appears anywhere in this repository; examples use documentation
ranges from RFC 5737.

## Requirements

- Go `1.26` or newer. The Go toolchain can download the required toolchain automatically when `GOTOOLCHAIN=auto` is enabled.
- Nothing else. No Python, no external tools, no cgo.

The native protocol dependency is pinned to the immutable pseudo-version in `go.mod`. It is an MIT-licensed, V3-focused implementation, tested for discovery, authentication, status reads and writes against the units this was developed on, but it is still a hardware-oriented community implementation. The library's own scope is narrower than a general Midea library, so mode/fan/temperature decoding beyond the fields exercised here should be treated as unvalidated. The controller interface is deliberately small so it can be replaced if a better Go implementation appears.

## One-time bootstrap

The Midea account is needed only to fetch each device's long-lived LAN token/key. It is not needed for normal local operation.

```sh
bin/midea-mcp-control bootstrap
```

The command prompts for the SmartHome account and password (the password is read
without echo), discovers the units, asks the cloud for a token/key per device,
**verifies each credential against the unit over the LAN**, and only then writes
`~/.config/midea-mcp-control/devices.json` with mode `0600`. A unit whose cloud
token does not authenticate is reported and skipped; the others are still
saved. An existing inventory is merged, never replaced blindly, and a file it
cannot parse is left untouched rather than overwritten.

```sh
# Check the cloud handshake without writing anything.
bin/midea-mcp-control bootstrap --dry-run

# Re-bootstrap a single re-paired unit.
bin/midea-mcp-control bootstrap --device bedroom
```

The password can also be supplied without prompting. The precedence is
flag, then environment, then stdin, then an interactive prompt (the password is
read without echo on a terminal):

```sh
# Both lines on stdin: account first, then password.
printf '%s\n%s\n' "$EMAIL" "$PASSWORD" | bin/midea-mcp-control bootstrap --credentials-stdin

# Account piped, password held out of band in the environment.
printf '%s\n' "$EMAIL" | MIDEA_PASSWORD="$PASSWORD" bin/midea-mcp-control bootstrap --credentials-stdin

# Account on the command line, password piped.
bin/midea-mcp-control bootstrap --account "$EMAIL" --credentials-stdin < password.txt
```

`--credentials-stdin` reads **only the credentials that are still missing**, one
per line, account first. Supplying the account elsewhere therefore means the
first line is the password. Empty input is rejected rather than treated as an
empty password. Neither value is ever written to disk, and the password is not
echoed. `bootstrap` is a CLI-only command by design: it is never reachable
through MCP.

Verify the resulting file permissions before use:

```sh
stat -c '%a %n' ~/.config/midea-mcp-control/devices.json
bin/midea-mcp-control audit
```

## Build

```sh
GOTOOLCHAIN=auto go build -o bin/midea-mcp-control ./cmd/midea-mcp-control
```

Or install it on your `PATH`:

```sh
CGO_ENABLED=0 GOTOOLCHAIN=auto go install github.com/mheers/midea-mcp-control/cmd/midea-mcp-control@latest
```

`CGO_ENABLED=0` is not required — the source contains no cgo — but it produces a
statically linked binary, which is what the pipeline builds. Without it `go
install` links the Go standard library's optional cgo paths for `net` and
`os/user` and you get a dynamically linked executable.

The default configuration path can be overridden with `MIDEA_MCP_CONTROL_CONFIG` or the `--config` flag.

## CLI

```sh
# Configured devices; no credentials are displayed.
bin/midea-mcp-control list

# Credential metadata and file permissions, without token/key values.
bin/midea-mcp-control audit

# Credential-free UDP discovery.
# The native Go discovery was run on this LAN and found all three units.
bin/midea-mcp-control discover --json

# Read all configured units.
bin/midea-mcp-control status

# What each unit says it supports.
bin/midea-mcp-control capabilities

# Power draw. The unit's own realtime field is always zero on this hardware,
# so the useful number is the average derived from lifetime kWh deltas.
bin/midea-mcp-control energy living-room --samples 4 --interval 75s

# Authenticate every unit using only the saved LAN credentials.
bin/midea-mcp-control verify

# Physical writes require an explicit confirmation flag.
bin/midea-mcp-control off bedroom --confirm
bin/midea-mcp-control on bedroom --confirm

# Any combination of fields, applied in one frame and read back.
bin/midea-mcp-control set bedroom --temp 21.5 --mode cool --fan low --confirm
bin/midea-mcp-control set living-room --mode auto --temp 23 --confirm

# One-time cloud bootstrap (prompts for the account password).
bin/midea-mcp-control bootstrap
```

A selector is an exact configured name, IP address, or numeric device ID. Boolean
flags accept `--eco true`, `--eco=true` and a bare `--eco` (meaning true).

### What the writes actually do on this hardware

Measured on all three units, not assumed. The boolean flags behave
differently when a unit is **off** and when it is **running**, so both columns
are real data:

| Field | Unit off | Unit running | Notes |
|---|---|---|---|
| power | yes | yes | verified by read-back |
| mode | yes | yes | `auto`, `cool`, `dry`, `heat`, `fan`, `smart_dry` |
| temperature | yes | yes | 17–30 °C in 0.5 °C steps |
| fan speed | yes | yes | `auto`, `silent`, `low`, `medium`, `high`, `full` |
| swing (vertical/horizontal) | **no** | **yes** | only honoured while running |
| sleep | no | transient | accepted once, then cleared by the unit |
| eco, turbo, display | no | **no** | accepted on the wire, refused by the hardware |

The refused fields stay exposed, because they are part of the protocol and may
work on other units. The tool reports the disagreement instead of a false
success:

```text
error: command sent to living-room but read-back disagrees: turbo is false, requested true
```

Two further observations from the running test:

- `eco` once returned `update: no response after 1 attempts`. That is the
  single-attempt write policy working as intended: the frame is never resent on
  a timeout, because a blind retry is worse than an honest failure.
- The `0x40` frame is full-state, so a later write seeded from a fresh read can
  roll back a flag the unit had silently cleared. That is why `sleep` looked
  set and then reverted.

### Write safety

A write seeds the device's current state, changes only the fields you named, and
is verified by an independent read-back. The frame carrying the command is
transmitted **at most once**; a read-back that disagrees with the request is an
error, not a warning. Because a unit can drop the response to a query that
immediately follows a set frame, verification runs on a fresh connection with
the read retry budget — this never re-sends the command.

The legacy `0x40` set frame is full-state, so firmware can normalize fields you
did not ask about; the adapter only requests the changes you specify.

## MCP over stdio

The default MCP command opens no network listener. Configure an MCP client with the built binary:

```json
{
  "mcpServers": {
    "midea": {
      "command": "/absolute/path/to/midea-mcp-control/bin/midea-mcp-control",
      "args": ["mcp", "--config", "/home/you/.config/midea-mcp-control/devices.json"]
    }
  }
}
```

Available tools:

- `list_devices` — list configured devices without secrets;
- `verify_credentials` — read-only check that authenticates every configured unit with stored LAN credentials and returns an independent result for each unit; the CLI `verify` command records successful `last_verified` metadata;
- `discover_devices` — read-only LAN broadcast discovery;
- `get_status` — read one configured device;
- `get_capabilities` — read a device's feature report;
- `get_energy` — read a device's power draw; the only objective way to tell what an operating mode is doing;
- `set_power` — physical on/off write; requires `confirm: true`, changes only power, and reads back the state;
- `set_state` — physical write for temperature, mode, fan, swing, eco, turbo, sleep or display; sparse (omitted fields are left alone), requires `confirm: true`, and every requested field is verified by read-back.

`confirm` is a guard against accidental calls, not an authorization boundary: an MCP client that is allowed to reach this local server can still set it to `true`. Keep write-capable MCP clients behind your own human-approval policy.

The MCP boundary does not accept raw tokens, keys, or protocol frames. Its optional discovery target is limited to a private IPv4/loopback address and carries only a fixed read-only probe; control operations only accept configured device selectors.

## MCP over HTTP

```sh
bin/midea-mcp-control mcp --http                     # 127.0.0.1:8765
bin/midea-mcp-control mcp --http --http-stateless    # no server-side sessions
bin/midea-mcp-control mcp --http --http-addr 127.0.0.1:9000
```

This server can change a physical device, so the HTTP transport is locked down
by default:

- **Bearer token required.** There is no way to start it without one. The token
  comes from `MIDEA_MCP_CONTROL_TOKEN`, or is generated once into
  `~/.config/midea-mcp-control/mcp-token` with mode `0600`. Only the *path* is
  printed, never the token.
- **Loopback only.** Binding `0.0.0.0` or a LAN address is refused unless you
  pass `--http-allow-remote`. Authentication and DNS-rebinding protection stay on
  even then.
- **DNS-rebinding protection.** The `Host` header must be a loopback name and a
  non-empty `Origin` must be a loopback origin. A hostile page that resolves to
  `127.0.0.1` therefore cannot drive the device from a browser.
- Token comparison is constant-time and the scheme is matched
  case-insensitively, per RFC 7235.

```sh
curl -H "authorization: Bearer $(cat ~/.config/midea-mcp-control/mcp-token)" \
     -H 'content-type: application/json' \
     -H 'accept: application/json, text/event-stream' \
     -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' \
     http://127.0.0.1:8765/mcp
```

If you do expose it beyond loopback, put it behind a reverse proxy that
terminates TLS, and treat the bearer token as a device credential.

## Token lifetime

There are two different values involved in V3 LAN authentication:

1. The cloud `getToken` response carries a 64-byte device token (128 hex
   characters) and a 32-byte key (64 hex characters). The response contains no
   expiry timestamp.
2. The device handshake derives a temporary per-connection local/TCP key. The
   `msmart-ng` reference implementation marks that derived key as valid for
   12 hours; that is not an expiry on the stored token or key. The Go adapter
   derives it for each short-lived CLI/MCP operation and does not cache it.

Measured on this LAN, the behaviour is:

- **The cloud mints a new token/key pair on every `getToken` call.** Three
  consecutive bootstraps minutes apart produced three different pairs for each
  unit. `bootstrap` reports this as `(rotated)`.
- **A previously issued pair keeps working.** After a new pair had been fetched
  and stored, the previous pair still authenticated all three units over the
  LAN. A pair first obtained hours earlier also still worked.
- **Therefore re-running `bootstrap` is safe** for any other consumer that
  holds an older pair (for example a Home Assistant integration); the device
  does not revoke the earlier credential. The file's contents do change, so
  keep a backup of a known-good inventory.

What can still invalidate a stored pair: re-pairing or factory-resetting a
unit, replacing its Wi-Fi module, or a cloud-side revocation. No credential
system can promise a vendor-issued secret survives those. The practical
protections here are:

- keep the mode-`0600` inventory file backed up offline;
- run `midea-mcp-control verify` periodically (for example, from a user-level timer) to detect invalidation; a simple hourly crontab entry is `0 * * * * /absolute/path/to/bin/midea-mcp-control verify --timeout 30s >/dev/null`;
- rerun `midea-mcp-control bootstrap` if a unit is re-paired or a verification fails;
- never commit the inventory or the bootstrap account credentials.

`midea-mcp-control audit` deliberately reports `expiry: none reported` rather than claiming an expiry the vendor never provided.

## Development

```sh
gofmt -w cmd/midea-mcp-control/*.go internal/*/*.go
go test ./...
go vet ./...
CGO_ENABLED=0 go build ./...
```

The tested seams are configuration permissions/validation, LAN discovery
parsing, cloud handshake signing and token retrieval (against a fake cloud
server and vectors captured from the reference implementation), the HTTP
transport's authentication and DNS-rebinding defences, and read/write command
behavior. The write path is tested with a fake protocol client;
physical-device acceptance was verified separately against all three units.

### CI as code with Dagger

The pipeline is a [Dagger](https://dagger.io) module written in Go, living in
`.dagger/`. It runs the same checks locally and in CI:

```sh
dagger call ci
```

| Function | What it enforces |
|---|---|
| `fmt` | no unformatted Go file |
| `constraints` | no `os/exec`, no `import "C"`, no `.py` file anywhere |
| `vet` | `go vet` with `CGO_ENABLED=0` |
| `test` | the unit suite with `CGO_ENABLED=0` |
| `race` | the suite under the race detector |
| `build` | cross-compiles linux/amd64, linux/arm64, darwin/arm64, windows/amd64 |

`constraints` exists because "pure Go" is a promise that is trivial to break by
accident and easy to miss in review. It is a real check, not a comment: the
pipeline was verified to fail on each of the three violations.

`.github/workflows/ci.yml` calls `dagger call ci` on pushes to `main` and on
pull requests, and passes **no secrets** — the pipeline is hermetic and never
touches a device, the inventory, or the vendor cloud. The generated Dagger
client under `.dagger/internal/` is not committed; the engine regenerates it.
