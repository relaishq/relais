#!/usr/bin/env python3
"""boot-signal: record this host's one template boot outcome, without a cloud API."""
import json
import sys
from pathlib import Path

try:
    scope = json.loads(Path("/rehearsal/scope.json").read_text())
    args = sys.argv[1:]
    if len(args) != 8 or args[0] != "-e" or args[2:] != [
        "--stack", scope["stack"], "--resource", scope["resource"],
        "--region", scope["region"],
    ] or not args[1].isdigit() or not 0 <= int(args[1]) <= 255:
        raise ValueError("outside scope")
    output = Path("/var/lib/rehearsal")
    output.mkdir(mode=0o700, exist_ok=True)
    with (output / "boot-status").open("x") as stream:
        stream.write(args[1] + "\n")
except (ValueError, KeyError, OSError):
    sys.stderr.write("local boot signal refused\n")
    sys.exit(254)
