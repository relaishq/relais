"""R-only restoration. Appended only to the two R service step scripts."""
def relay_loss_execute(op, p, evidence=None):
    if op == "relay-loss-stop":
        unit = p["activeUnit"]
        if unit not in ("relais-relay.service", "relais-relay-standby.service"):
            raise ValueError("active relay unit unknown")
        old = int(command(["systemctl", "show", "--value", "-p", "MainPID", unit]))
        if old == 0:
            # systemd retains the last main PID after a failed relay exits.
            old = int(command(["systemctl", "show", "--value", "-p", "ExecMainPID", unit]))
        else:
            # Check listener ownership without an HTTP read that could race
            # self-exit. Only the requested unit's PID may be stopped.
            address = urlparse(p["relay"]).netloc
            matches = re.findall(re.escape(address) + r"\s+.*?pid=(\d+)", command(["ss", "-H", "-lntp"]))
            if matches == [str(old)]:
                command(["systemctl", "stop", unit])
            else:
                # A self-exiting relay closes its listener before systemd
                # records the exit. Wait briefly; never stop another unit.
                for _ in range(50):
                    if command(["systemctl", "show", "--value", "-p", "MainPID", unit]) == "0":
                        break
                    time.sleep(0.1)
                else:
                    raise ValueError("active relay changed" if matches else "active relay unit unknown")
        # Relay units use Restart=no. Never stop a different active unit or
        # infer recovery here: R's rejoin loop checks epoch, workers and health.
        if command(["systemctl", "show", "--value", "-p", "MainPID", unit]) != "0" or old <= 0 or pathlib.Path(f"/proc/{old}").exists():
            raise ValueError("relay predecessor exit unconfirmed")
        return {"completed": True}
    if op != "relay-start" or p.get("slot") != "relay-two":
        raise ValueError("relay-start requires relay-two")
    unit = "relais-relay.service"
    old = int(command(["systemctl", "show", "--value", "-p", "MainPID", unit]))
    if old > 0:
        command(["systemctl", "stop", unit])
    if command(["systemctl", "show", "--value", "-p", "MainPID", unit]) != "0" or old > 0 and pathlib.Path(f"/proc/{old}").exists():
        raise ValueError("relay predecessor exit unconfirmed")
    configure(unit, "relay", p["targetDir"])
    command(["systemctl", "start", unit])
    return {"completed": True}
