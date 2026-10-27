#!/usr/bin/env python3
"""template-metadata and secret-source: local files, exact listed boot reads only.

Installed at the compatibility path expected by the unchanged template. This
program has no SDK, network client, metadata endpoint or subprocess fallback.
"""
import json
import sys
from pathlib import Path

root = Path(__file__).resolve().parent
try:
    scope = json.loads((root / "scope.json").read_text())
    args = sys.argv[1:]
    command = args[:2]
    pairs = args[2:]
    if len(pairs) % 2:
        raise ValueError("invalid options")
    options = dict(zip(pairs[::2], pairs[1::2]))
    if len(options) * 2 != len(pairs):
        raise ValueError("duplicate options")
    if command == ["cloudformation", "describe-stack-resource"]:
        name = options.get("--logical-resource-id")
        if name not in scope["metadata"] or options != {
            "--stack-name": scope["stack"], "--logical-resource-id": name,
            "--region": scope["region"], "--query": "StackResourceDetail.Metadata",
            "--output": "text",
        }:
            raise ValueError("outside metadata scope")
        value = (root / "metadata" / (name + ".json")).read_text()
    elif command == ["secretsmanager", "get-secret-value"]:
        name = options.get("--secret-id")
        if name not in scope["secrets"] or options != {
            "--secret-id": name, "--region": scope["region"],
            "--query": "SecretString", "--output": "text",
        }:
            raise ValueError("outside secret scope")
        value = (root / "secrets" / name).read_text()
    else:
        raise ValueError("unsupported read")
    sys.stdout.write(value + "\n")
except (ValueError, KeyError, TypeError, OSError):
    sys.stderr.write("local template read refused\n")
    sys.exit(254)
