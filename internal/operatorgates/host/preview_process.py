"""Fixed public operator steps. No mutation has a retry loop."""
import json
import os
import re
import signal
import time
from urllib.parse import urlparse
import pathlib
import socket
import ssl
import subprocess
import urllib.request


def command(args):
    return subprocess.run(args, check=True, capture_output=True, text=True, timeout=30).stdout.strip()


def http(url, method="GET"):
    # Private literal endpoints are supplied from the captured stack, never URLs
    # from API responses. Do not forward requests through ambient proxies.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(urllib.request.Request(url, method=method), timeout=5) as r:
        body = r.read(256_001)
        if len(body) > 256_000:
            raise ValueError("oversized observation")
        return json.loads(body) if body else {}


def control_password(path=pathlib.Path("/run/relais/control.env")):
    """Read the prepared role credential as data, never as executable shell."""
    info = path.stat()
    if info.st_uid != 0 or info.st_mode & 0o077:
        raise ValueError("control credential permissions")
    rows = [line.split("=", 1)[1] for line in path.read_text().splitlines()
            if line.startswith("RELAIS_REDIS_PASSWORD=")]
    if len(rows) != 1 or not re.fullmatch(r"[A-Za-z0-9]{64}", rows[0]):
        raise ValueError("control credential invalid")
    return rows[0]


class RedisRead:
    def __init__(self, host, port, password, name):
        self.sock = ssl.create_default_context().wrap_socket(socket.create_connection((host, port), timeout=3), server_hostname=name)
        self.file = self.sock.makefile("rb")
        self.call("AUTH", "control", password)

    def read(self):
        line = self.file.readline(8192)
        if not line.endswith(b"\r\n"):
            raise ValueError("incomplete store read")
        kind, body = line[:1], line[1:-2]
        if kind == b"-":
            raise ValueError("store read refused")
        if kind == b"+":
            return body.decode()
        n = int(body)
        if kind == b":":
            return n
        if n == -1:
            return None
        if n < 0 or n > 65536:
            raise ValueError("oversized store observation")
        if kind == b"*":
            return [self.read() for _ in range(n)]
        if kind == b"$":
            data = self.file.read(n + 2)
            if len(data) != n + 2 or not data.endswith(b"\r\n"):
                raise ValueError("incomplete bulk observation")
            return data[:-2].decode()
        raise ValueError("invalid store observation")

    def call(self, *args):
        parts = [str(a).encode() for a in args]
        self.sock.sendall(b"*%d\r\n" % len(parts) + b"".join(b"$%d\r\n" % len(a) + a + b"\r\n" for a in parts))
        return self.read()

    def scan(self, pattern):
        cursor, keys = "0", set()
        for _ in range(64):
            cursor, page = self.call("SCAN", cursor, "MATCH", pattern, "COUNT", "128")
            keys.update(page)
            if len(keys) > 8192:
                raise ValueError("store inventory too large")
            if cursor == "0":
                return keys
        raise ValueError("store inventory incomplete")

    def close(self):
        self.file.close()
        self.sock.close()


def ownership(p):
    password = control_password()
    seed = RedisRead(p["store"], p["storePort"], password, p["store"])
    clients = []
    counts = {"w1": 0, "w2": 0}
    intents = 0
    try:
        masters = {(r[2][0] or p["store"], r[2][1]) for r in seed.call("CLUSTER", "SLOTS")}
        if len(masters) != 1:
            raise ValueError("not the reviewed one-shard store")
        for host, port in masters:
            c = RedisRead(host, port, password, p["store"])
            clients.append(c)
            sec, usec = c.call("TIME")
            now = int(sec) * 1000 + int(usec) // 1000
            for key in c.scan("relais:preview:lease:*"):
                worker, expires = c.call("HMGET", key, "worker", "expires")
                if worker is None and expires is None:
                    continue  # Key disappeared during a read, not a failed read.
                if int(expires) > now:
                    matches = [w for w in counts if p["workers"][w]["media"] == worker]
                    if len(matches) != 1:
                        raise ValueError("unknown authoritative owner")
                    counts[matches[0]] += 1
            intents += len(c.scan("relais:preview:control-intent:*"))
        return {"owners": counts, "transfers": intents}
    finally:
        seed.close()
        for c in clients:
            c.close()


def control_draining(control, worker):
    # The placement fence lives in control's registration. A control-driven drain never
    # sets the worker process's own /status draining flag: only its SIGTERM path does.
    return any(w["name"] == worker and w["draining"] is True for w in control["workers"])


def empty_worker(p, worker):
    """Pure observations only: the worker's /status and raw store reads. Control /status can reconcile
    ended calls, so the driver reads it in its own step and checks control's side there."""
    local = http(p["workers"][worker]["http"] + "/status")
    store = ownership(p)
    if store["transfers"] != 0:
        raise ValueError("store transfer pending")
    if store["owners"][worker] != 0:
        raise ValueError("store still owns calls on worker")
    if local["inventory_known"] is not True or local["truncated"] is not False or local["sessions"] != 0 or local["inventory"] != []:
        raise ValueError("worker local inventory not empty")


# Fixed short codes for a failed pure read-only step. Free-text messages never leave the host.
READ_FAILURES = {"oversized observation": "oversized", "incomplete store read": "store-incomplete",
                 "store read refused": "store-refused", "oversized store observation": "store-oversized",
                 "incomplete bulk observation": "store-incomplete-bulk", "invalid store observation": "store-invalid",
                 "store inventory too large": "store-too-large", "store inventory incomplete": "store-scan-incomplete",
                 "not the reviewed one-shard store": "store-topology", "unknown authoritative owner": "store-unknown-owner",
                 "store transfer pending": "store-transfer", "store still owns calls on worker": "store-owner",
                 "worker local inventory not empty": "local-inventory", "active relay unit unknown": "relay-unit-unknown"}


def read_failure(error):
    """A fixed refusal code, or the exception class name only (SSLCertVerificationError: ssl-cert-verification-error)."""
    if isinstance(error, ValueError) and str(error) in READ_FAILURES:
        return READ_FAILURES[str(error)]
    name = re.sub(r"(?<=[a-z0-9])(?=[A-Z])|(?<=[A-Z])(?=[A-Z][a-z])", "-", type(error).__name__).lower()
    return name if re.fullmatch(r"[a-z][a-z0-9-]{0,39}", name) else "unclassified"


def execute(op, p):
    if op == "telemetry":
        return telemetry(p)
    worker = p.get("worker")
    if op == "move":
        status = http(p["control"] + "/status")
        calls = [c for c in status["calls"] if c["id"] == p["session"]]
        if len(calls) != 1 or calls[0]["owner"] != p["owner"] or calls[0]["lease_epoch"] != p["epoch"]:
            raise ValueError("trial ownership changed")
        return http(p["control"] + "/calls/" + p["session"] + "/move?to=" + worker, "POST")
    if op == "status":
        return http(p["control"] + "/status")
    if op == "store-status":
        return ownership(p)
    if op == "worker-status":
        return http(p["workers"][worker]["http"] + "/status")
    if op == "relay-status":
        return http(p["relay"] + "/status")
    if op in ("freeze", "unfreeze", "signal-stop", "signal-start"):
        if op == "signal-start":
            configure("relais-signal.service", "signal", p["targetDir"])
        command(["systemctl", "stop" if op in ("freeze", "signal-stop") else "start", "relais-signal.service"])
        if op in ("freeze", "signal-stop") and command(["systemctl", "show", "--value", "-p", "MainPID", "relais-signal.service"]) != "0":
            raise ValueError("admissions not frozen")
    elif op == "drain":
        http(p["control"] + "/private/workers/" + worker + "/mark-draining", "POST")
        return http(p["control"] + "/workers/" + worker + "/drain", "POST")
    elif op == "worker-empty":
        empty_worker(p, worker)
    elif op == "worker-stop":
        local = http(p["workers"][worker]["http"] + "/status")
        if local["sessions"] != 0 or local["inventory"] != [] or not local["inventory_known"] or local["truncated"] or not control_draining(http(p["control"] + "/status"), worker):
            raise ValueError("worker no longer empty")
        old = int(command(["systemctl", "show", "--value", "-p", "MainPID", "relais-worker.service"]))
        if old <= 0:
            raise ValueError("missing worker predecessor")
        command(["systemctl", "stop", "relais-worker.service"])
        if command(["systemctl", "show", "--value", "-p", "MainPID", "relais-worker.service"]) != "0" or pathlib.Path(f"/proc/{old}").exists():
            raise ValueError("worker predecessor not exited")
    elif op == "replace":
        c = http(p["control"] + "/status")
        w = next(x for x in c["workers"] if x["name"] == worker)
        if not w["dead"] or w["recovering"] or w["call_count"] != 0:
            raise ValueError("worker death not settled")
        return http(p["control"] + "/private/workers/" + worker + "/replace", "POST")
    elif op in ("worker-start", "control-start"):
        role = "worker" if op == "worker-start" else "control"
        configure("relais-" + role + ".service", role, p["targetDir"])
        started = time.monotonic()
        command(["systemctl", "start", "relais-" + role + ".service"])
        if role == "control":
            while time.monotonic() - started < 120:
                try:
                    if http(p["control"] + "/status")["ready"] is True:
                        return {"completed": True, "readyMs": int((time.monotonic() - started) * 1000)}
                except (OSError, ValueError):
                    pass
                time.sleep(0.1)
            raise ValueError("control reconciliation missing")
    elif op in ("control-stop", "worker-down"):
        role = "control" if op == "control-stop" else "worker"
        unit = "relais-" + role + ".service"
        old = int(command(["systemctl", "show", "--value", "-p", "MainPID", unit]))
        if old <= 0:
            raise ValueError("missing predecessor")
        if op == "worker-down":
            before, after = kill_main(unit, old)
        else:
            before = time.time_ns()
            command(["systemctl", "stop", unit])
            after = time.time_ns()
        for _ in range(50):
            if command(["systemctl", "show", "--value", "-p", "MainPID", unit]) == "0" and not pathlib.Path(f"/proc/{old}").exists():
                break
            time.sleep(0.1)
        else:
            raise ValueError("predecessor exit unconfirmed")
        return {"completed": True, "signalBeforeUnixNs": str(before), "signalAfterUnixNs": str(after)}
    elif op == "relay-inspect":
        units = {u: int(command(["systemctl", "show", "--value", "-p", "MainPID", u])) for u in ("relais-relay.service", "relais-relay-standby.service")}
        address = urlparse(p["relay"]).netloc
        matches = re.findall(re.escape(address) + r"\s+.*?pid=(\d+)", command(["ss", "-H", "-lntp"]))
        active = [u for u, pid in units.items() if str(pid) in matches and pid > 0]
        if len(active) != 1:
            raise ValueError("active relay unit unknown")
        return {"active": active[0], "pids": units, "status": http(p["relay"] + "/status")}
    elif op in ("relay-stage", "relay-replenish"):
        observed = execute("relay-inspect", p)
        unit = next(u for u in observed["pids"] if u != observed["active"])
        old = observed["pids"][unit]
        command(["systemctl", "stop", unit])
        if command(["systemctl", "show", "--value", "-p", "MainPID", unit]) != "0" or old > 0 and pathlib.Path(f"/proc/{old}").exists():
            raise ValueError("standby predecessor exit unconfirmed")
        configure(unit, "relay-standby", p["targetDir"])
        started = int(time.time())
        command(["systemctl", "start", unit])
        until = time.monotonic() + 15
        while time.monotonic() < until:
            pid = command(["systemctl", "show", "--value", "-p", "MainPID", unit])
            log = command(["journalctl", "-u", unit, "--since", "@" + str(started), "--no-pager", "-o", "cat"])
            if pid.isdigit() and int(pid) > 0 and f"STANDBY_WAIT pid={pid} lease=preview" in log:
                return {"completed": True}
            time.sleep(0.1)
        raise ValueError("standby waiting evidence missing")
    elif op == "relay-stop":
        observed = execute("relay-inspect", p)
        if observed["active"] != p["activeUnit"]:
            raise ValueError("active relay changed")
        old = observed["pids"][observed["active"]]
        command(["systemctl", "stop", observed["active"]])
        if command(["systemctl", "show", "--value", "-p", "MainPID", observed["active"]]) != "0" or pathlib.Path(f"/proc/{old}").exists():
            raise ValueError("relay predecessor exit unconfirmed")
    else:
        raise ValueError("unsupported fixed process step")
    return {"completed": True}


def kill_main(unit, pid):
    """SIGKILL the unit's exact main process and bracket only that signal.

    systemd resolves the target and later confirms its exit; a subprocess around
    the signal would widen the bracket past the approved clock mapping. The pidfd
    pins the process, so a reused PID can never receive the signal.
    """
    fd = os.pidfd_open(pid)
    try:
        if int(command(["systemctl", "show", "--value", "-p", "MainPID", unit])) != pid:
            raise ValueError("predecessor changed before signal")
        before = time.time_ns()
        signal.pidfd_send_signal(fd, signal.SIGKILL)
        after = time.time_ns()
    finally:
        os.close(fd)
    return before, after


def configure(unit, role, target):
    directory = pathlib.Path("/etc/systemd/system") / (unit + ".d")
    directory.mkdir(mode=0o755, exist_ok=True)
    (directory / "preview.conf").write_text("[Service]\nEnvironment=RELAIS_BIN_DIR=" + target + "\nExecStart=\nExecStart=/opt/relais/boot/run.sh " + role + "\n")
    command(["systemctl", "daemon-reload"])
