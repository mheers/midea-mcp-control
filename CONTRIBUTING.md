# Contributing

Thanks for looking. This is a small, self-contained Go project and the bar is
"obvious and verifiable".

## Ground rules

- **Pure Go.** No Python, no `os/exec`, no cgo. The Dagger pipeline fails the
  build if any of those appear, so this is enforced rather than aspirational.
- **Never commit credentials.** The device inventory, the MCP bearer token, and
  any cloud account details live outside the repository under the user's config
  directory. `.gitignore` covers the obvious paths, but the real protection is
  not adding them in the first place.
- **No real deployment data.** Use RFC 5737 addresses (`192.0.2.0/24`,
  `198.51.100.0/24`, `203.0.113.0/24`) and invented device ids in tests and
  documentation.

## Before you open a pull request

```sh
dagger call ci
```

That runs gofmt, `go vet`, the test suite, the race detector, a four-target
cross-compile, and the pure-Go constraint check. If you do not have Dagger
installed, the equivalent is:

```sh
gofmt -l cmd internal
go vet ./...
go test ./...
go test -race ./...
CGO_ENABLED=0 go build ./...
```

## Testing expectations

Every change needs a test at the seam it affects. The existing seams are:

| Seam | Where |
|---|---|
| inventory safety and validation | `internal/config` |
| cloud handshake and token retrieval | `internal/cloud` |
| read/write device behaviour | `internal/controller` |
| LAN discovery, including a real socket | `internal/discovery` |
| orchestration and credential bootstrap | `internal/bootstrap` |
| MCP tools, HTTP auth, rebinding defence | `internal/mcpserver` |
| argument and credential handling | `cmd/midea-control` |

Tests must not talk to a real appliance, the real cloud, or a real network.
Fakes are used at every such boundary, including a loopback UDP responder for
discovery.

## Hardware-dependent claims

If you change anything about the protocol, say how you know it works. A claim
like "mode X is supported" is only useful if it was exercised on a unit, and
the answer can legitimately differ between a powered-on and powered-off
appliance — as it did for the swing flags.
