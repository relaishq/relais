"""Bounded host isolation. Kernel expiry also works without a user session."""
import fcntl
import json
import subprocess
import shutil
import uuid

ISOLATION_TABLE = "relais_gate_isolation"
ISOLATION_LOCK = "/run/relais-gate-isolation.lock"


def isolation_rules(seconds, generation):
    # Management, resolver and link-local clock/credential traffic survive. A generic
    # established rule would also preserve existing store and private HTTP flows.
    # create only the table itself. Explicit add commands avoid relying on
    # create-table nested-body expansion; nft commits this whole batch atomically.
    # nft 1.0.4 exposes set comments in JSON, but omits table comments.
    lines = [f'create table inet {ISOLATION_TABLE}',
             f'add set inet {ISOLATION_TABLE} isolated {{ type nf_proto; flags timeout; comment "{generation}"; elements = {{ ipv4 timeout {seconds}s, ipv6 timeout {seconds}s }}; }}']
    for chain in ("input", "output"):
        lines.append(f"add chain inet {ISOLATION_TABLE} {chain} {{ type filter hook {chain} priority -10; policy accept; }}")
        inbound = chain == "input"
        address = "saddr" if inbound else "daddr"
        rules = [('iifname' if inbound else 'oifname') + ' "lo" accept',
                 f"ip {address} 169.254.0.0/16 accept",
                 f"ip6 {address} fe80::/10 accept",
                 "meta l4proto ipv6-icmp icmpv6 type { 133, 134, 135, 136 } accept"]
        for protocol, port in (("udp", 53), ("tcp", 53), ("tcp", 443)):
            if inbound:
                rules.append(f"{protocol} sport {port} ct direction reply ct state established accept")
            else:
                rules.append(f"{protocol} dport {port} ct direction original accept")
        rules.append("meta nfproto @isolated drop")
        lines.extend(f"add rule inet {ISOLATION_TABLE} {chain} {rule}" for rule in rules)
    return "\n".join(lines) + "\n"



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
 if r.returncode == 0 and any(x.get('set',{{}}).get('family') == 'inet' and x['set'].get('table') == {ISOLATION_TABLE!r} and x['set'].get('name') == 'isolated' and x['set'].get('comment') == {generation!r} for x in json.loads(r.stdout)['nftables']):
  subprocess.run([{nft!r}, 'delete', 'table', 'inet', {ISOLATION_TABLE!r}],check=True,timeout=5)
'''
        command(["systemd-run", "--quiet", "--collect", "--unit=relais-gate-isolation-" + generation,
                 "--on-active=" + str(seconds) + "s", "--timer-property=AccuracySec=1s",
                 "--timer-property=RandomizedDelaySec=0", "--property=RuntimeMaxSec=15s",
                 "/usr/bin/python3", "-c", cleanup])
        subprocess.run([nft, "-f", "-"], input=isolation_rules(seconds, generation),
                       text=True, capture_output=True, check=True, timeout=10)
    return {"completed": True}
