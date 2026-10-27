"""Read-only, bounded telemetry from the captured lean hosts."""
def telemetry(p):
    slot = p["slot"]
    endpoint = p["workers"][slot]["http"] if slot in ("w1", "w2") else p["control"] if slot == "shared" else p["relay"]
    local, ages = None, []
    if slot in ("w1", "w2"):
        try:
            status = http(endpoint + "/status")
            local = {key: status[key] for key in ("sessions", "inventory_known", "truncated", "inventory")}
            for session in p.get("sessions", []):
                if not re.fullmatch(r"[A-Za-z0-9-]{1,128}", session):
                    raise ValueError("invalid observed session")
                try:
                    agent = http(endpoint + "/status?agent-session=" + session)["agent_session"]
                    ages.append({key: agent[key] for key in ("checkpoint_known", "checkpoint_age_ms", "snapshot_age_ms")})
                except (OSError, ValueError, KeyError):
                    ages.append(None)
        except (OSError, ValueError, KeyError):
            local = None
    metrics = None
    try:
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open(endpoint + "/metrics", timeout=5) as response:
            raw = response.read(256001)
            if len(raw) > 256000:
                raise ValueError("oversized metrics")
        metrics = [line for line in raw.decode().splitlines() if re.match(r"relais_(redis_|store_|transport_|checkpoint_|worker_agent_|relay_)", line)]
    except (OSError, ValueError):
        pass
    ena, network = {}, {}
    for device in pathlib.Path("/sys/class/net").iterdir():
        if device.name == "lo" or not re.fullmatch(r"[A-Za-z0-9_-]{1,32}", device.name):
            continue
        try:
            network[device.name] = {name: int((device / "statistics" / name).read_text()) for name in ("rx_bytes", "tx_bytes")}
            raw = command(["ethtool", "-S", device.name])
            counters = {}
            for name in ("bw_in_allowance_exceeded", "bw_out_allowance_exceeded", "pps_allowance_exceeded", "conntrack_allowance_exceeded", "linklocal_allowance_exceeded"):
                found = re.search(r"^\s*" + name + r":\s*([0-9]+)\s*$", raw, re.M)
                counters[name] = int(found[1]) if found else None
            ena[device.name] = counters
        except (OSError, ValueError, subprocess.SubprocessError):
            ena[device.name] = None
    return {"at": int(time.time() * 1000), "metrics": metrics, "ena": ena, "network": network, "local": local, "ages": ages}
