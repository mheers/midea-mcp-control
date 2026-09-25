#!/usr/bin/env python3
"""Bootstrap and verify long-lived Midea V3 LAN credentials.

This is intentionally a one-time helper. Normal control is local and uses the
Go CLI/MCP server with the saved token/key; the Midea account is not needed at
runtime. Run with, for example:

    MIDEA_ACCOUNT='...' MIDEA_PASSWORD='...' \
      uv run --with 'midea-local==12.1.0' python scripts/bootstrap.py

If the environment variables are absent, the script prompts without echoing the
password. It never writes the account password and never prints token/key data.
"""

from __future__ import annotations

import argparse
import asyncio
import getpass
import json
import logging
import os
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import aiohttp
from midealocal.cloud import get_midea_cloud
from midealocal.const import ProtocolVersion
from midealocal.devices import device_selector
from midealocal.discover import discover

DEFAULT_CLOUD = "SmartHome"
CONFIG_ENV = "MIDEA_CONTROL_CONFIG"


def default_config_path() -> Path:
    configured = os.environ.get(CONFIG_ENV)
    if configured:
        return Path(configured).expanduser()
    return Path.home() / ".config" / "midea-control" / "devices.json"


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, default=default_config_path())
    parser.add_argument("--cloud-name", default=DEFAULT_CLOUD)
    parser.add_argument(
        "--device", help="limit bootstrap to one configured name, IP, or device id"
    )
    parser.add_argument("--timeout", type=float, default=5.0)
    return parser.parse_args()


def credentials() -> tuple[str, str]:
    account = os.environ.get("MIDEA_ACCOUNT", "")
    password = os.environ.get("MIDEA_PASSWORD", "")
    if not account:
        account = input("Midea account: ").strip()
    if not password:
        password = getpass.getpass("Midea password: ")
    if not account or not password:
        raise RuntimeError("both account and password are required")
    return account, password


def load_existing(path: Path) -> dict[str, Any]:
    if not path.exists():
        return {"format": 1, "devices": []}
    info = path.stat()
    if info.st_mode & 0o077:
        raise RuntimeError(
            f"config permissions are too broad: {info.st_mode & 0o777:04o}"
        )
    data = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise TypeError("existing config must be a JSON object")
    if not isinstance(data.get("devices", []), list):
        raise TypeError("existing config devices must be a list")
    data.setdefault("format", 1)
    return data


def save(path: Path, data: dict[str, Any]) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            descriptor = -1
            handle.write(json.dumps(data, indent=2) + "\n")
            handle.flush()
            os.fsync(handle.fileno())
        temporary.replace(path)
        os.chmod(path, 0o600)
    finally:
        if descriptor >= 0:
            os.close(descriptor)
        temporary.unlink(missing_ok=True)


def public_state(attributes: dict[str, Any]) -> dict[str, Any]:
    keys = (
        "power",
        "mode",
        "target_temperature",
        "indoor_temperature",
        "outdoor_temperature",
        "fan_speed",
    )
    return {key: attributes[key] for key in keys if key in attributes}


async def run(args: argparse.Namespace) -> int:
    account, password = credentials()
    existing = load_existing(args.config)
    discovered = discover()
    if not discovered:
        print("No Midea devices answered LAN discovery.", file=sys.stderr)
        return 2

    by_id = {int(item["device_id"]): item for item in discovered.values()}
    unsupported = [item for item in by_id.values() if item["protocol"] != 3]
    for item in unsupported:
        print(
            f"Skipping {item['ip_address']}: native inventory supports V3 only.",
            file=sys.stderr,
        )
    by_id = {
        device_id: item for device_id, item in by_id.items() if item["protocol"] == 3
    }
    if not by_id:
        print("No V3 Midea devices answered LAN discovery.", file=sys.stderr)
        return 2
    if args.device:
        selector = args.device.casefold()
        by_id = {
            device_id: item
            for device_id, item in by_id.items()
            if selector
            in {
                str(device_id).casefold(),
                str(item["ip_address"]).casefold(),
                str(item.get("name", "")).casefold(),
            }
        }
        if len(by_id) != 1:
            print(
                f"Device selector {args.device!r} did not match exactly one unit.",
                file=sys.stderr,
            )
            return 2
    print("Discovered local devices:")
    for item in sorted(by_id.values(), key=lambda value: value["ip_address"]):
        print(
            f"  {item['ip_address']}:{item['port']} "
            f"id={item['device_id']} model={item['model']} protocol=V{item['protocol']}"
        )

    now = datetime.now(timezone.utc).isoformat()
    updated: dict[str, dict[str, Any]] = {}
    failures: list[str] = []
    async with aiohttp.ClientSession() as session:
        cloud = get_midea_cloud(args.cloud_name, session, account, password)
        if not await cloud.login():
            print("Cloud login failed.", file=sys.stderr)
            return 3
        appliances = await cloud.list_appliances(None) or {}
        for device_id, local in sorted(by_id.items()):
            label = f"{local['ip_address']} ({appliances.get(device_id, {}).get('name', 'unnamed')})"
            try:
                keys = await cloud.get_cloud_keys(device_id)
            except Exception as exc:  # noqa: BLE001 - one cloud failure must not hide other units
                failures.append(label)
                print(f"  credential lookup failed for {label}: {exc}", file=sys.stderr)
                continue
            selected: dict[str, str] | None = None
            selected_method: int | None = None
            state: dict[str, Any] | None = None
            for method, pair in keys.items():
                device = device_selector(
                    name=appliances.get(device_id, {}).get("name")
                    or local.get("name")
                    or local["ip_address"],
                    device_id=device_id,
                    device_type=local["type"],
                    ip_address=local["ip_address"],
                    port=local["port"],
                    token=pair["token"],
                    key=pair["key"],
                    device_protocol=ProtocolVersion(local["protocol"]),
                    model=local["model"],
                    subtype=0,
                    customize="",
                    mac=local.get("mac"),
                    serial_number=local.get("sn"),
                )
                if device is None:
                    continue
                try:
                    if device.connect(check_protocol=True):
                        selected = pair
                        selected_method = method
                        state = public_state(device.attributes)
                        break
                except Exception as exc:  # noqa: BLE001 - candidate libraries expose several error types
                    print(
                        f"  candidate {method} failed for {label}: {exc}",
                        file=sys.stderr,
                    )
                finally:
                    device.close()
            if selected is None:
                failures.append(label)
                print(f"No working LAN token/key for {label}.", file=sys.stderr)
                continue
            updated[str(device_id)] = {
                "id": str(device_id),
                "name": appliances.get(device_id, {}).get("name")
                or local.get("name")
                or local["ip_address"],
                "ip": local["ip_address"],
                "port": local["port"],
                "type": local["type"],
                "model": local["model"],
                "protocol": local["protocol"],
                "mac": local.get("mac"),
                "token": selected["token"],
                "key": selected["key"],
                "credential_method": selected_method,
                "obtained_at": now,
                "last_verified": now,
                # The app-facing V3 response contains no expiry field. These
                # credentials are re-authenticated on every local connection.
                "expiry": None,
                "state_at_verification": state,
            }
            print(f"  verified {label}")

    merged = {
        str(item.get("id")): item
        for item in existing.get("devices", [])
        if isinstance(item, dict) and item.get("id") is not None
    }
    merged.update(updated)
    updated_ids = set(updated)
    used_names = {
        str(item.get("name", "")).casefold()
        for item in existing.get("devices", [])
        if isinstance(item, dict) and str(item.get("id")) not in updated_ids
    }
    for key in sorted(merged):
        item = merged[key]
        name = str(item.get("name") or item["ip"])
        if name.casefold() in used_names:
            name = f"{name} ({item['ip']})"
        item["name"] = name
        used_names.add(name.casefold())
    existing["format"] = 1
    existing["devices"] = [merged[key] for key in sorted(merged)]
    if updated:
        save(args.config, existing)
        print(
            f"Saved {len(updated)} verified device credentials to {args.config} (mode 0600)."
        )
    if failures:
        print(
            f"{len(failures)} unit(s) could not be verified: {', '.join(failures)}",
            file=sys.stderr,
        )
        return 4
    return 0


def main() -> int:
    logging.basicConfig(level=logging.ERROR)
    return asyncio.run(run(parse_args()))


if __name__ == "__main__":
    raise SystemExit(main())
