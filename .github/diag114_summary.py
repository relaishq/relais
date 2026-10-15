"""Summarize #114 diagnostic JSON lines. Temporary investigation helper."""

import glob
import json
import os
import statistics
import sys
from datetime import datetime


def ts(value):
    if not value or value.startswith("0001-"):
        return None
    # Go RFC 3339 with nanoseconds; trim to microseconds for Python.
    head, _, tail = value.partition(".")
    if tail:
        digits = ""
        rest = ""
        for i, ch in enumerate(tail):
            if ch.isdigit():
                digits += ch
            else:
                rest = tail[i:]
                break
        value = head + "." + (digits + "000000")[:6] + rest
    if value.endswith("Z"):
        value = value[:-1] + "+00:00"
    return datetime.fromisoformat(value)


def ms(a, b):
    if a is None or b is None:
        return None
    return round((a - b).total_seconds() * 1000, 1)


def fmt(v):
    return "-" if v is None else str(v)


def takeover_view(t):
    r = t.get("result", {})
    return {
        "from": t.get("from"),
        "to": t.get("to"),
        "lost": bool(t.get("lost")),
        "cause": t.get("loss_cause", ""),
        "error": t.get("error", ""),
        "snap_ms": round(t.get("snapshot_age", 0) / 1e6, 1),
        "margin": t.get("sequence_margin"),
        "policy": t.get("checkpoint_policy", ""),
        "detect_ms": ms(ts(t.get("detected_at")), ts(t.get("last_heartbeat"))),
        "dur_ms": ms(ts(t.get("end")), ts(t.get("start"))),
        "replay_ms": round(r.get("RelayReplayDuration", 0) / 1e6, 1),
        "replay_pkts": r.get("RelayReplayPackets"),
        "replay_complete": r.get("RelayReplayComplete"),
        "hold_expired": r.get("HoldExpired"),
        "start": ts(t.get("start")),
        "end": ts(t.get("end")),
    }


def main(directory, only=""):
    rows = []
    for path in sorted(glob.glob(os.path.join(directory, "*.jsonl"))):
        pending = []
        for line in open(path):
            rec = json.loads(line)
            if rec["event"] in ("takeover-timeout", "takeover-late"):
                pending.append(rec)
                continue
            if rec["event"] != "outcome":
                continue
            if only and not rec["test"].endswith(only):
                pending = []
                continue
            marks = {k: ts(v) for k, v in (rec.get("marks") or {}).items()}
            row = {"pid": rec["pid"], "failed": rec.get("failed", False), "kind": "pass"}
            status = rec.get("status")
            timeout = next((p for p in pending if p["event"] == "takeover-timeout"), None)
            late = next((p for p in pending if p["event"] == "takeover-late"), None)
            pending = []
            if timeout is not None:
                status = timeout.get("status")
                row["kind"] = "window"
                row["n_at_3s"] = len((timeout.get("status") or {}).get("takeovers") or [])
                late_at = ts(late.get("late_at")) if late else None
                row["late_ms"] = ms(late_at, ts(timeout.get("wait_start")))
                row["late_n"] = late.get("late_count") if late else None
                if late and late.get("status"):
                    status = late["status"]
                workers = (timeout.get("status") or {}).get("workers") or []
                row["dead_at_3s"] = ",".join(w["name"] for w in workers if w.get("dead"))
            takeovers = [takeover_view(t) for t in ((status or {}).get("takeovers") or [])]
            if takeovers:
                last = takeovers[-1]
                if row["kind"] == "pass" and row["failed"]:
                    row["kind"] = "lost" if last["lost"] else "other"
                row["t"] = takeovers
            row["boundary_ms"] = ms(marks.get("boundary"), marks.get("kill_a"))
            row["wait_ms"] = ms(marks.get("wait_done"), marks.get("wait_start"))
            relay = rec.get("relay") or {}
            row["relay"] = {k: relay.get(k) for k in ("HoldTimeouts", "BufferExpired", "BufferDrops", "ReplayExpiries", "Replays")}
            rows.append(row)

    kinds = {}
    for row in rows:
        kinds[row["kind"]] = kinds.get(row["kind"], 0) + 1
    print(f"runs={len(rows)} breakdown={kinds}")
    print("kind | n@3s late_ms late_n dead@3s | boundary_ms wait_ms | T1 replay_ms/pkts | T2 from>to lost cause snap_ms margin policy detect_ms dur_ms replay_ms/pkts complete hold_exp | error | relay")
    for row in rows:
        t = row.get("t", [])
        t1 = t[0] if t else {}
        t2s = [x for x in t[1:]]
        t2 = t2s[-1] if t2s else {}
        extra = "" if len(t) <= 2 else f" EXTRA_TAKEOVERS={len(t)} " + ";".join(f"{x['from']}>{x['to']} lost={x['lost']} {x['cause']}" for x in t)
        if row["kind"] == "pass" and not row["failed"]:
            pass
        print(" | ".join([
            row["kind"] + ("*" if row["failed"] else ""),
            f"{fmt(row.get('n_at_3s'))} {fmt(row.get('late_ms'))} {fmt(row.get('late_n'))} {fmt(row.get('dead_at_3s'))}",
            f"{fmt(row.get('boundary_ms'))} {fmt(row.get('wait_ms'))}",
            f"{fmt(t1.get('replay_ms'))}/{fmt(t1.get('replay_pkts'))}",
            f"{fmt(t2.get('from'))}>{fmt(t2.get('to'))} {fmt(t2.get('lost'))} {fmt(t2.get('cause'))} {fmt(t2.get('snap_ms'))} {fmt(t2.get('margin'))} {fmt(t2.get('policy'))} {fmt(t2.get('detect_ms'))} {fmt(t2.get('dur_ms'))} {fmt(t2.get('replay_ms'))}/{fmt(t2.get('replay_pkts'))} {fmt(t2.get('replay_complete'))} {fmt(t2.get('hold_expired'))}",
            (t2.get("error") or "")[:160] + extra,
            json.dumps(row["relay"]),
        ]))

    def dist(name, values):
        values = [v for v in values if v is not None]
        if not values:
            return
        values.sort()
        print(f"{name}: n={len(values)} min={values[0]} p50={statistics.median(values)} p90={values[int(0.9 * (len(values) - 1))]} max={values[-1]}")

    for kind in sorted(kinds):
        sel = [r for r in rows if r["kind"] == kind]
        print(f"-- {kind}")
        dist("  T2 snapshot_ms", [r["t"][-1]["snap_ms"] for r in sel if len(r.get("t", [])) >= 2])
        dist("  T2 detect_ms", [r["t"][-1]["detect_ms"] for r in sel if len(r.get("t", [])) >= 2])
        dist("  T2 dur_ms", [r["t"][-1]["dur_ms"] for r in sel if len(r.get("t", [])) >= 2])
        dist("  T2 replay_ms", [r["t"][-1]["replay_ms"] for r in sel if len(r.get("t", [])) >= 2])
        dist("  T1 replay_ms", [r["t"][0]["replay_ms"] for r in sel if r.get("t")])
        dist("  boundary_ms", [r.get("boundary_ms") for r in sel])
        dist("  wait_ms", [r.get("wait_ms") for r in sel])
        dist("  late_ms", [r.get("late_ms") for r in sel])


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2] if len(sys.argv) > 2 else "")
