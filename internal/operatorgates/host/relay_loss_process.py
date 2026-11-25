"""R-only restoration. Appended only to the two R service step scripts."""
def relay_loss_execute(op, p, evidence=None):
    if op == "relay-loss-stop":
        return execute("relay-stop", p, evidence)
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
