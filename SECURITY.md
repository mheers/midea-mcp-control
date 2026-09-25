# Security policy

## What this tool holds

`midea-control` reads a V3 LAN token and key for each configured appliance.
Those are long-lived device credentials: anyone holding one can read and change
that unit's settings from the LAN. Treat the inventory file as a secret.

The file lives at `~/.config/midea-control/devices.json` with mode `0600`, and
the loader refuses to read it if the permissions are broader, if it is a
symlink, or if it cannot be parsed. The CLI and MCP tools never print a token
or key, and no credential is committed to this repository.

The HTTP transport additionally stores a bearer token at
`~/.config/midea-control/mcp-token` with mode `0600`.

## Defaults that matter

- MCP over **stdio** opens no network listener at all. Prefer it.
- MCP over **HTTP** refuses to start without a bearer token, binds loopback
  only unless `--http-allow-remote` is given, and validates both the `Host` and
  `Origin` headers to block DNS rebinding.
- Write tools require an explicit `confirm: true` and verify every requested
  field by reading the device back. A disagreement is reported as an error, not
  a success.
- The frame carrying a physical write is transmitted at most once; a timed-out
  command is never resent automatically.
- The cloud account password is used only during `bootstrap`, is never written
  to disk, and is not reachable through MCP.

## Reporting a vulnerability

Open a private security advisory on the repository rather than a public issue.
Please include the version or commit, what an attacker gains, and how to
reproduce it. Do not include a real token, key, or device address in the
report; a sanitised description is enough to fix the issue.

## Things a maintainer should be careful about

- The protocol dependency is a community implementation pinned to one commit.
  A version bump can change wire behaviour with no changelog.
- The Midea cloud endpoints are undocumented and can change without notice.
  `bootstrap` may stop working; local control will not.
- Do not relax the HTTP protections to "just for debugging" without a
  corresponding note in the README.
