"""Bounded host isolation. Kernel expiry also works without a user session."""
import fcntl
import json
import subprocess
import shutil
import uuid

ISOLATION_TABLE = "relais_gate_isolation"
ISOLATION_LOCK = "/run/relais-gate-isolation.lock"


def isolation_rules(seconds, generation):
    # Only outbound management HTTPS and its reply direction survive. A generic
    # established rule would also preserve existing store and private HTTP flows.
    return f'''create table inet {ISOLATION_TABLE} {{
 comment "{generation}"
 set isolated {{ type nf_proto; flags timeout; elements = {{ ipv4 timeout {seconds}s, ipv6 timeout {seconds}s }}; }}
 chain input {{ type filter hook input priority -10; policy accept;
  iifname "lo" accept
  tcp sport 443 ct direction reply ct state established accept
  meta nfproto @isolated drop
 }}
 chain output {{ type filter hook output priority -10; policy accept;
  oifname "lo" accept
  tcp dport 443 ct direction original accept
  meta nfproto @isolated drop
 }}
}}
'''


def isolation_table(nft, command):
    tables = json.loads(command([nft, "-j", "list", "tables"]))["nftables"]
    return any(row.get("table", {}).get("family") == "inet" and row["table"].get("name") == ISOLATION_TABLE for row in tables)


def isolation_restore(nft, command):
    if isolation_table(nft, command):
        command([nft, "delete", "table", "inet", ISOLATION_TABLE])


def relay_isolation(op, p, command):
    nft = shutil.which("nft")
    if nft is None:
        raise ValueError("nft required for relay isolation")
    seconds = p.get("isolationSeconds", 120)
    if type(seconds) is not int or not 1 <= seconds <= 300:
        raise ValueError("invalid isolation duration")
    with open(ISOLATION_LOCK, "a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        if op == "relay-restore":
            isolation_restore(nft, command)
            return {"completed": True}
        if isolation_table(nft, command):
            raise ValueError("relay already isolated")
        generation = uuid.uuid4().hex
        # Arm before applying rules. A late application still has its own kernel
        # timeout. The generation check prevents an old timer removing a new fault.
        cleanup = f'''import fcntl,json,subprocess
with open({ISOLATION_LOCK!r}, 'a') as lock:
 fcntl.flock(lock, fcntl.LOCK_EX)
 r=subprocess.run([{nft!r}, '-j', 'list', 'table', 'inet', {ISOLATION_TABLE!r}],capture_output=True,text=True,timeout=5)
 if r.returncode == 0 and any(x.get('table',{{}}).get('comment') == {generation!r} for x in json.loads(r.stdout)['nftables']):
  subprocess.run([{nft!r}, 'delete', 'table', 'inet', {ISOLATION_TABLE!r}],check=True,timeout=5)
'''
        command(["systemd-run", "--quiet", "--collect", "--unit=relais-gate-isolation-" + generation,
                 "--on-active=" + str(seconds) + "s", "--timer-property=AccuracySec=1s",
                 "--timer-property=RandomizedDelaySec=0", "--property=RuntimeMaxSec=15s",
                 "/usr/bin/python3", "-c", cleanup])
        subprocess.run([nft, "-f", "-"], input=isolation_rules(seconds, generation),
                       text=True, capture_output=True, check=True, timeout=10)
    return {"completed": True}
