"""Fail-closed summary of matched observations from one disposable runner."""
import math
from datetime import datetime
import re

CASES = [("before", "steady"), ("after", "steady"), ("before", "echo"),
         ("after", "echo"), ("after", "history-churn"), ("after", "rotation"),
         ("after", "n-plus-one"), ("after", "drain")]


def finite(value):
    return isinstance(value, (float, int)) and not isinstance(value, bool) and math.isfinite(value) and value >= 0


def summarize(data, cpu_us, wall_us):
    blocking, rows = [], []
    if data.get("kind") != "split-capacity" or data.get("outcome") != "complete":
        blocking.append("matched run incomplete")
    if data.get("goos") != "linux":
        blocking.append("Mac/local smoke only; Linux thread CPU / hosted evidence not established")
    builds = data.get("binary_sha256", {})
    names = {"relais-load", "relais-worker", "delayed-loopback", "relais-relay", "relais-control"}
    if not data.get("runner") or set(builds) != names or any(not isinstance(v, str) or not re.fullmatch(r"[0-9a-f]{64}", v) for v in builds.values()):
        blocking.append("runner or exact five-binary build evidence missing")
    if data.get("store_commands_budget_per_call_second") != 100 or data.get("store_bytes_budget_per_call_second") != 131072:
        blocking.append("unchanged store ceilings missing")
    steps = data.get("steps", [])
    identities = [(s.get("measurement", {}).get("arm"), s.get("measurement", {}).get("scenario")) for s in steps]
    if identities != CASES:
        blocking.append("missing, duplicate, or reordered matched/scenario steps")
    matched = {}
    for step in steps:
        m, r = step.get("measurement", {}), step.get("run", {})
        arm, scenario = m.get("arm"), m.get("scenario")
        label = f"{arm}/{scenario}"
        bad = lambda reason: blocking.append(f"{label}: {reason}")
        # The old 8 KiB override is expected to exceed the store byte ceiling.
        # Preserve that failure, while refusing every other failure in either arm.
        causes = step.get("causes", [])
        before_budget_only = arm == "before" and step.get("service_outcome") == "fail" and causes and all(
            c == "store budget exceeded; checkpoint cadence requires follow-up before operating-load approval" for c in causes)
        if step.get("service_outcome") != "pass" and not before_budget_only:
            bad("service gate did not pass: " + "; ".join(causes))
        if step.get("generator_outcome") != "valid":
            bad("generator validity missing")
        expected_args = ["-agent-split-checkpoints"] if arm == "after" else ([] if scenario == "echo" else ["-agent-max-snapshot-bytes=24576", "-agent-snapshot-bytes-per-second=270336", "-agent-snapshot-burst-bytes=98304"])
        if (m.get("worker_args") or []) != expected_args:
            bad("worker overrides do not match the accepted arms")
        calls = r.get("callers")
        if calls != data.get("callers") or not finite(calls) or calls < 1:
            bad("unmatched call load")
        if scenario in ("steady", "echo"):
            if r.get("duration_ns") != data.get("duration_seconds", 0) * 1e9 or step.get("relay_buffer_enabled") is not True:
                bad("duration or default relay-buffer evidence missing")
            matched[(arm, scenario)] = (calls, r.get("duration_ns"), step.get("relay_buffer_enabled"))
        if m.get("live_calls") != calls or m.get("lost_calls") != 0 or sum(m.get("survivors", {}).values()) != calls:
            bad("live-call / no-loss survivor evidence missing")
        if scenario == "n-plus-one" and ("0" in m.get("survivors", {}) or len(m.get("survivors", {})) > 2):
            bad("N+1 calls did not all survive on the two remaining workers")
        leaks = r.get("leaks", {})
        if not leaks.get("checked") or leaks.get("errors") or leaks.get("processes") or leaks.get("listeners"):
            bad("cleanup gate did not pass")
        ages = m.get("ages", {})
        if not ages.get("coverage_complete"):
            bad("100ms checkpoint/snapshot age coverage incomplete")
        for field in ("checkpoint_age_p99_ms", "snapshot_age_p99_ms"):
            if not finite(ages.get(field)) or ages[field] > 550:
                bad(field + " missing or over 550ms")
        for field in ("checkpoint_age_max_ms", "snapshot_age_max_ms"):
            if not finite(ages.get(field)):
                bad(field + " missing")
        custom = step.get("custom_agent")
        if scenario != "echo":
            custom = custom or {}
            if custom.get("evidence_verdict") != "pass":
                bad("custom evidence incomplete: " + "; ".join(custom.get("blocking_approval", [])))
            for field in ("checkpoint_age_p99_ms", "snapshot_age_p99_ms"):
                value = custom.get(field)
                if not finite(value) or value > 550:
                    bad(field + " missing or exceeds 550ms")
            for field in ("checkpoint_age_max_ms", "snapshot_age_max_ms"):
                if not finite(custom.get(field)):
                    bad(field + " missing")
            for field, budget in (("save_cpu_seconds_per_event", cpu_us), ("save_wall_seconds_per_event", wall_us)):
                value = custom.get(field)
                if not finite(value) or value * 1e6 > budget:
                    bad(field + " missing or over operator budget")
            if not finite(custom.get("admission_calls")) or custom.get("admission_calls", 0) <= 0 or custom.get("admission_cpu_unavailable") != 0:
                bad("separate state-admission CPU evidence unavailable")
            if not custom.get("checkpoint_skips") or not finite(custom.get("admission_wall_seconds")) or not finite(custom.get("admission_cpu_seconds")):
                bad("admission timing or skip reasons missing")
            if scenario == "steady" and (custom.get("ForcedSuccesses", 0) <= 0 or custom.get("ForcedFailures") != 0 or custom.get("forced_max_ms", 0) <= 0):
                bad("forced intent checkpoint count/latency proof missing")
        if scenario == "history-churn" and (m.get("history_refusals", 0) <= 0 or m.get("fixture_samples", 0) <= 0):
            bad("history churn not refused before acceptance")
        if scenario == "rotation":
            if m.get("inventory_live_calls") != calls or m.get("live_key_id_inventory") != {"2": 5 * calls}:
                bad("authenticated live tuple inventory is not exclusively on key 2")
            if len(r.get("worker_rejoins", [])) != 3 or any(not valid_rejoin(w) for w in r.get("worker_rejoins", [])):
                bad("three rolling replacements did not prove rejoin")
        trace = m.get("store", {})
        phases = trace.get("phases", {})
        if trace.get("errors"):
            bad("store observer errors: " + "; ".join(trace["errors"]))
        phase_rows = {}
        wanted = ["setup", "steady"] + (["rotation"] if scenario == "rotation" else [])
        wanted += [p for p in phases if p not in wanted and p not in ("cleanup", "hangup")]
        for phase in wanted:
            values = phases.get(phase, {})
            seconds = trace.get("phase_seconds", {}).get(phase)
            if not finite(seconds) or not seconds or (not values.get("commands") and phase in ("setup", "steady", "rotation")) or any(not finite(values.get(k)) for k in ("commands", "request_bytes", "reply_bytes")):
                bad(phase + " traffic window missing")
                continue
            denom = seconds * calls
            phase_rows[phase] = {**values, "seconds": seconds,
                                 "commands_per_call_second": values["commands"] / denom,
                                 "request_reply_bytes_per_call_second": (values["request_bytes"] + values["reply_bytes"]) / denom,
                                 "component_writes_per_call_second": {k: v["writes"] / denom for k, v in values.get("components", {}).items()},
                                 "component_bytes_per_call_second": {k: v["bytes"] / denom for k, v in values.get("components", {}).items()}}
            if arm == "after" and scenario != "echo" and phase in ("setup", "steady", "rotation"):
                if set(values.get("key_arguments", {})) != {f"{i:02d}" for i in range(1, 12)} or not valid_keys(values.get("key_examples", [])):
                    bad(phase + " exact 11-key argument evidence missing")
                if not all(k in values.get("hset_field_values", {}) for k in ("checkpoint:b", "checkpoint:m", "checkpoint:t")):
                    bad(phase + " actual HSET field evidence missing")
                t = values.get("hset_field_values", {}).get("checkpoint:t", {})
                # Initialization writes an empty placeholder before publication.
                if t.get("max_bytes") != 8 or t.get("min_bytes") not in (0, 8):
                    bad(phase + " store-clock field disagrees with frozen eight-byte format")
        for field, ceiling in (("store_commands_per_call_second", 100), ("store_bytes_per_call_second", 131072)):
            value = step.get(field)
            if not finite(value) or (arm == "after" and value > ceiling):
                bad(field + " missing or over ceiling")
        media = [v for p, v in phase_rows.items() if p != "setup"]
        media_seconds = sum(p["seconds"] for p in media)
        if media_seconds:
            media_commands = sum(p["commands"] for p in media) / (media_seconds * calls)
            media_bytes = sum(p["request_bytes"] + p["reply_bytes"] for p in media) / (media_seconds * calls)
            if arm == "after" and (media_commands > 100 or media_bytes > 131072):
                bad("observed complete media-window attempts exceed store ceiling")
        else:
            media_commands = media_bytes = None
        duration_ns = r.get("duration_ns")
        lifecycle_denominator = duration_ns / 1e9 * calls if finite(duration_ns) and duration_ns > 0 and finite(calls) and calls > 0 else 0
        lifecycle_commands = lifecycle_bytes = None
        if lifecycle_denominator:
            lifecycle_commands = sum(v.get("commands", 0) for v in phases.values()) / lifecycle_denominator
            lifecycle_bytes = sum(v.get("request_bytes", 0) + v.get("reply_bytes", 0) for v in phases.values()) / lifecycle_denominator
            if arm == "after" and (lifecycle_commands > 100 or lifecycle_bytes > 131072):
                bad("all observed lifecycle attempts, including cold loads/setup/hangup/cleanup, exceed media-call-second ceiling")
        else:
            bad("lifecycle traffic denominator missing")
        seconds = trace.get("seconds", {})
        peak = max((v.get("request_bytes", 0) + v.get("reply_bytes", 0) for v in seconds.values()), default=None)
        rows.append({"arm": arm, "scenario": scenario, "service": step.get("service_outcome"),
                     "baseline_over_budget": bool(before_budget_only), "causes": causes,
                     "callers": calls, "phases": phase_rows, "observed_media_commands_per_call_second": media_commands,
                     "observed_media_bytes_per_call_second": media_bytes,
                     "lifecycle_commands_per_media_call_second": lifecycle_commands,
                     "lifecycle_bytes_per_media_call_second": lifecycle_bytes, "peak_observed_second_bytes": peak,
                     "store_bytes_by_client": step.get("store_bytes_by_client_per_call_second"),
                     "custom_agent": custom, "measurement": m,
                     "events": r.get("events"), "yardstick": r.get("yardstick"),
                     "worker_peaks": r.get("worker_peak_calls"), "worker_rejoins": r.get("worker_rejoins"),
                     "cleanup": leaks})
    for scenario in ("steady", "echo"):
        if matched.get(("before", scenario)) != matched.get(("after", scenario)) or matched.get(("after", scenario)) is None:
            blocking.append(scenario + ": call load, duration or relay buffers do not match")
    return {"approval": "BLOCKING APPROVAL" if blocking else "EVIDENCE READY FOR OWNER REVIEW; approval is manual",
            "scope": "One runner's measurements; no independent-host, managed-store, billing, NIC or durable-log claim.",
            "runner": data.get("runner"), "build": data.get("binary_sha256"),
            "blocking": sorted(set(blocking)), "steps": rows}


def valid_rejoin(worker):
    try:
        start = parse_time(worker["started_at"])
        end = parse_time(worker["completed_at"])
        heartbeat = parse_time(worker["last_heartbeat"])
        return end >= start and heartbeat > start and worker["pid"] > 0 and worker["old_pid"] > 0 and worker["pid"] != worker["old_pid"]
    except (KeyError, TypeError, ValueError):
        return False


def valid_keys(keys):
    if len(keys) != 11 or not all(isinstance(k, str) for k in keys) or ":lease:" not in keys[0]:
        return False
    prefix, tag = keys[0].split("lease:", 1)
    names = ["lease:", "record:", "epoch:", "routes:", "ended:", "setup:", "checkpoint:"]
    expected = [prefix + name + tag for name in names]
    expected += [prefix + kind + ":" + tag + ":" + str(bank) for kind in ("work", "history") for bank in (0, 1)]
    return keys == expected


def parse_time(value):
    # Go emits RFC3339Nano with variable-width fractions. Python 3.9 accepts
    # only three or six fraction digits; preserve ordering at microsecond scale.
    value = re.sub(r"\.(\d+)(?=Z|[+-]\d\d:\d\d|$)", lambda m: "." + (m.group(1) + "000000")[:6], value)
    return datetime.fromisoformat(value.replace("Z", "+00:00"))
