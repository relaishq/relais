"""Read only public metadata, unit definitions and linked binary identities.

Never open an EnvironmentFile, /proc/*/environ, or a credential file.
"""
import hashlib
import json
import pathlib
import re
import subprocess
import time

# Fixed short codes for known refusals. Free-text messages never leave the host.
CATEGORIES = {"node topology differs": "topology", "stable target process not running": "not-running",
              "unknown service configuration": "service-config", "installed manifest not pinned": "manifest",
              "initial archive receipt differs": "archive", "staged archive receipt differs": "archive",
              "unreviewed service arguments": "boot-script", "unreviewed unit start": "unit-start",
              "running inode differs from installed binary": "binary", "unknown worker budgets or profile": "budgets",
              "linked build identity differs": "build-identity"}
UNITS = {"w1": [("w1", "worker")], "w2": [("w2", "worker")],
         "relay": [("relay-primary", "relay"), ("relay-standby", "relay")],
         "shared": [("control", "control"), ("signal", "signal")]}


def collect(slot, expected, root=pathlib.Path("/"), run=None, require_running=False):
    if run is None:
        def run(args):
            return subprocess.run(args, check=True, capture_output=True, text=True, timeout=10).stdout.strip()
    node = json.loads((root / "etc/relais/node.json").read_text())
    if node["architecture"] != "arm64" or set(node["components"]) != {r for _, r in UNITS[slot]}:
        raise ValueError("node topology differs")
    rows = []
    for name, role in UNITS[slot]:
        suffix = "relay-standby" if name == "relay-standby" else role
        unit = "relais-" + suffix + ".service"
        pid = int(run(["systemctl", "show", "--value", "-p", "MainPID", unit]))
        if require_running and pid <= 0:
            raise ValueError("stable target process not running")
        # The only permitted explicit environment override chooses a staged,
        # independently verified binary directory. EnvironmentFiles are not read.
        environment = run(["systemctl", "show", "--value", "-p", "Environment", unit])
        if environment and not re.fullmatch(r'RELAIS_BIN_DIR=/opt/relais/preview/[0-9]+\.[0-9]+\.[0-9]+', environment):
            raise ValueError("unknown service configuration")
        directory = environment.split("=", 1)[1] if environment else "/opt/relais/bin"
        installed = root / directory.lstrip("/")
        manifest_bytes = (installed / "compatibility.json").read_bytes()
        manifest = json.loads(manifest_bytes)
        pin = next((p for p in expected if p["version"] == manifest["version"] and p["commit"] == manifest["commit"]), None)
        if pin is None or hashlib.sha256(manifest_bytes).hexdigest() != pin["manifestSha256"]:
            raise ValueError("installed manifest not pinned")
        if not environment and node["sha256"] != pin["archiveSha256"]:
            raise ValueError("initial archive receipt differs")
        if environment and (installed / "archive.sha256").read_text().strip() != pin["archiveSha256"]:
            raise ValueError("staged archive receipt differs")
        boot = (root / "opt/relais/boot/run.sh").read_bytes()
        if hashlib.sha256(boot).hexdigest() != pin["bootSha256"]:
            raise ValueError("unreviewed service arguments")
        unit_text = run(["systemctl", "cat", unit])
        effective = []
        for start in re.findall(r'^ExecStart=(.*)$', unit_text, re.M):
            if not start:
                effective = []
            else:
                effective.append(start)
        allowed = ["/opt/relais/boot/run.sh " + suffix]
        if role == "relay":
            allowed.append("/opt/relais/boot/run.sh relay-standby")
        if len(effective) != 1 or effective[0] not in allowed:
            raise ValueError("unreviewed unit start")
        binary = installed / ("relais-" + role)
        if pid:
            actual = (root / f"proc/{pid}/exe").resolve(strict=True)
            if actual != binary.resolve(strict=True):
                raise ValueError("running inode differs from installed binary")
            args = (root / f"proc/{pid}/cmdline").read_bytes().decode().split("\0")
            # The reviewed boot script enables no overrides. Refuse any split or
            # snapshot/move budget flag even if the process kept its old name.
            if any(re.search(r'split|snapshot|checkpoint|move-memory|agent-(?!echo)', a) for a in args):
                raise ValueError("unknown worker budgets or profile")
            binary = root / f"proc/{pid}/exe"
        identity = run([str(binary), "-version"])
        if not identity.startswith(f'relais-{role} version={pin["version"]} commit={pin["commit"]} date='):
            raise ValueError("linked build identity differs")
        rows.append(dict(name=name, role=role, profile="legacy" if role == "worker" else "default",
                         version=pin["version"], commit=pin["commit"], archive_sha256=pin["archiveSha256"],
                         agents=["echo"] if role == "worker" else [], default_budgets=role == "worker", manifest=manifest))
    return {"participants": rows, "observedAt": time.time_ns() // 1_000_000}


def failure_category(error):
    """A sanitized code for an inventory failure: a fixed refusal or the exception class."""
    if isinstance(error, ValueError) and str(error) in CATEGORIES:
        return CATEGORIES[str(error)]
    name = re.sub(r"(?<!^)(?=[A-Z])", "-", type(error).__name__).lower()
    return name if re.fullmatch(r"[a-z][a-z-]{0,29}", name) else "unclassified"
