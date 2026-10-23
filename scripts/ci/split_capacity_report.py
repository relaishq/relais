"""Fail-closed summary of matched observations from one disposable runner."""
import math
from datetime import datetime
import re

CASES = [("before", "steady"), ("after", "steady"), ("before", "echo"),
         ("after", "echo"), ("after", "history-churn"), ("after", "rotation"),
         ("after", "n-plus-one"), ("after", "drain")]


def finite(value):
    return isinstance(value, (float, int)) and not isinstance(value, bool) and math.isfinite(value) and value >= 0


# Observer-relative monotonic boundaries may differ by at most one microsecond.
CLOCK_TOLERANCE_SECONDS = 1e-6
WIRE_COUNTERS = ("commands", "request_bytes", "reply_bytes")


def trace_totals(trace):
    totals = {}
    for group in ("seconds", "phases"):
        rows = trace.get(group)
        if not isinstance(rows, dict) or not rows:
            raise ValueError(group + " traffic evidence missing")
        total = dict.fromkeys(WIRE_COUNTERS, 0)
        for key, row in rows.items():
            if group == "seconds" and (not isinstance(key, str) or not key.isdigit() or str(int(key)) != key):
                raise ValueError("seconds bin index invalid")
            for field in WIRE_COUNTERS:
                value = row.get(field) if isinstance(row, dict) else None
                if not isinstance(value, int) or isinstance(value, bool) or value < 0:
                    raise ValueError(group + " " + field + " must be nonnegative integers")
                total[field] += value
        totals[group] = total
    return totals


def validate_phase_tiling(trace):
    phases, durations, intervals = trace.get("phases", {}), trace.get("phase_seconds", {}), trace.get("phase_intervals", {})
    if not isinstance(durations, dict) or not isinstance(intervals, dict) or not set(phases).issubset(durations) or set(durations) != set(intervals):
        raise ValueError("phase interval coverage missing or inconsistent")
    timeline, elapsed = [], 0
    # A marked phase with no requests has intervals/duration but no traffic row.
    # Counter conservation still checks every recorded request and reply.
    for phase in durations:
        duration, parts = durations[phase], intervals[phase]
        if not finite(duration) or duration <= 0 or not isinstance(parts, list) or not parts:
            raise ValueError("phase duration or intervals invalid")
        measured = 0
        for part in parts:
            if not isinstance(part, dict):
                raise ValueError("phase interval bounds invalid")
            start, end = part.get("start"), part.get("end")
            if not finite(start) or not finite(end) or end <= start:
                raise ValueError("phase interval bounds invalid")
            timeline.append((start, end))
            measured += end - start
        if abs(measured - duration) > CLOCK_TOLERANCE_SECONDS:
            raise ValueError("phase intervals disagree with measured duration")
        elapsed += duration
    cursor = 0
    for start, end in sorted(timeline):
        if start < cursor - CLOCK_TOLERANCE_SECONDS:
            raise ValueError("phase intervals overlap")
        if start > cursor + CLOCK_TOLERANCE_SECONDS:
            raise ValueError("phase intervals have a gap")
        cursor = end
    if abs(cursor - elapsed) > CLOCK_TOLERANCE_SECONDS:
        raise ValueError("phase intervals do not cover the complete step")
    return elapsed


def rolling_phase(trace, phase, calls):
    """Use only complete one-second bins wholly inside one phase interval."""
    intervals = trace.get("phase_intervals", {}).get(phase)
    if not isinstance(intervals, list) or not intervals or not finite(calls) or calls <= 0:
        raise ValueError("phase boundaries or call denominator missing")
    seconds = trace.get("seconds", {})
    windows, full_bins = [], 0
    previous_end, duration = -1, 0
    for interval in intervals:
        start, end = interval.get("start"), interval.get("end")
        if not finite(start) or not finite(end) or end <= start or start < previous_end:
            raise ValueError("phase boundaries invalid or overlapping")
        previous_end = end
        duration += end - start
        first, stop = math.ceil(start), math.floor(end)
        # Bound malformed input before iterating; supported studies last <=65s.
        if stop - first > 3600:
            raise ValueError("phase interval exceeds bounded study")
        values = []
        for index in range(first, stop):
            value = seconds.get(str(index))
            if not isinstance(value, dict) or any(not finite(value.get(k)) for k in ("request_bytes", "reply_bytes")):
                raise ValueError("full phase bins missing or invalid")
            values.append(value["request_bytes"] + value["reply_bytes"])
        full_bins += len(values)
        for offset in range(len(values) - 9):
            windows.append(dict(first_bin=first + offset, last_bin=first + offset + 9,
                                request_reply_bytes_per_call_second=sum(values[offset:offset+10]) / (10 * calls)))
    measured_duration = trace.get("phase_seconds", {}).get(phase)
    if not finite(measured_duration) or not math.isclose(duration, measured_duration, rel_tol=0, abs_tol=CLOCK_TOLERANCE_SECONDS):
        raise ValueError("phase boundaries disagree with measured duration")
    return dict(full_bins=full_bins, window_count=len(windows), window_seconds=10,
                status="measured" if windows else f"sustained window not measured ({full_bins} full bins)",
                worst=max(windows, key=lambda w: w["request_reply_bytes_per_call_second"]) if windows else None)


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
        # The explicit legacy whole-blob override is expected to exceed the store byte ceiling.
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
        cap, rate, burst = (6144, 70000, 40960) if arm == "after" else ((7168, 81920, 28672) if scenario == "echo" else (24576, 270336, 98304))
        budgets = dict(max_snapshot_bytes=cap, snapshot_bytes_per_second=rate, snapshot_burst_bytes=burst,
                       move_bytes_per_session=1048576, move_bytes_total=67108864,
                       checkpoint_age_p99_ms=550, snapshot_age_p99_ms=550)
        if m.get("operator_budget_error"):
            bad("operator budgets unknown: " + str(m["operator_budget_error"]))
        elif m.get("operator_budgets") != budgets:
            bad("resolved worker budgets missing or incorrect")
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
        if scenario != "echo":
            if m.get("fixture_environment") != {"RELAIS_HISTORY_FULL_SIZE": "1"}:
                bad("full-size fixture selection missing")
            if arm == "after":
                sizes = m.get("fixture_sizes", {})
                restored = 0
                for size in sizes.values():
                    pair = (size.get("restored_working_bytes"), size.get("restored_history_bytes"))
                    if size.get("working_bytes") != 3072 or size.get("history_bytes") != 8192 or pair not in ((0, 0), (3072, 8192)):
                        bad("full working/history or Restore sizes incorrect")
                    restored += pair == (3072, 8192)
                # The matched planned move covers one call. Kill/drain place all
                # calls on the affected worker; rotation replaces every worker.
                required_restores = calls if scenario in ("n-plus-one", "drain", "rotation") else 1
                if len(sizes) != calls or restored < required_restores:
                    bad("full working/history or required Restore witnesses missing")
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
        try:
            totals = trace_totals(trace)
        except ValueError as error:
            bad("store trace invalid: " + str(error))
            rows.append(dict(arm=arm, scenario=scenario, service="fail" if step.get("service_outcome") == "fail" else "inconclusive", phases={}, trace_valid=False))
            continue
        for field in WIRE_COUNTERS:
            if totals["seconds"][field] != totals["phases"][field]:
                bad("seconds/phases " + field + " totals differ")
        try:
            validate_phase_tiling(trace)
        except (ValueError, TypeError, AttributeError) as error:
            bad("store trace invalid: " + str(error))
            rows.append(dict(arm=arm, scenario=scenario, service="fail" if step.get("service_outcome") == "fail" else "inconclusive", phases={}, trace_valid=False))
            continue
        phases = dict(phases)
        for phase in trace.get("phase_seconds", {}):
            phases.setdefault(phase, dict.fromkeys(WIRE_COUNTERS, 0))
        phase_rows = {}
        phase_failure = False
        wanted = ["setup", "steady"] + (["rotation"] if scenario == "rotation" else [])
        if scenario == "history-churn":
            wanted.append("churn")
        wanted += [p for p in phases if p not in wanted and p not in ("cleanup", "hangup")]
        for phase in wanted:
            values = phases.get(phase, {})
            seconds = trace.get("phase_seconds", {}).get(phase)
            if not finite(seconds) or not seconds or (not values.get("commands") and phase in ("setup", "steady", "rotation", "churn")) or any(not finite(values.get(k)) for k in ("commands", "request_bytes", "reply_bytes")):
                bad(phase + " traffic window missing")
                continue
            denom = seconds * calls
            phase_rows[phase] = {**values, "seconds": seconds,
                                 "commands_per_call_second": values["commands"] / denom,
                                 "request_reply_bytes_per_call_second": (values["request_bytes"] + values["reply_bytes"]) / denom,
                                 "component_writes_per_call_second": {k: v["writes"] / denom for k, v in values.get("components", {}).items()},
                                 "component_bytes_per_call_second": {k: v["bytes"] / denom for k, v in values.get("components", {}).items()}}
            if arm == "after":
                try:
                    rolling = rolling_phase(trace, phase, calls)
                    phase_rows[phase]["rolling_10_seconds"] = rolling
                    main_phase = "churn" if scenario == "history-churn" else "steady"
                    if phase == main_phase and rolling["worst"] is None:
                        bad(phase + " has fewer than 10 contiguous full bins")
                    if rolling["worst"] and rolling["worst"]["request_reply_bytes_per_call_second"] > 131072:
                        bad(phase + " sustained 10-second attempts exceed store ceiling")
                        phase_failure = True
                except (ValueError, TypeError, AttributeError) as error:
                    bad(phase + " sustained traffic evidence invalid: " + str(error))
            if phase == "churn":
                duration = data.get("duration_seconds")
                if not finite(duration) or duration <= 0 or seconds < duration - 10:
                    bad("churn traffic window too short")
                    phase_failure = True
                if phase_rows[phase]["commands_per_call_second"] > 100 or phase_rows[phase]["request_reply_bytes_per_call_second"] > 131072:
                    bad("churn attempts exceed store ceiling")
                    phase_failure = True
            if arm == "after" and scenario != "echo" and phase in ("setup", "steady", "rotation", "churn"):
                # Both six- and eleven-ARGV split commits still send eleven KEYS.
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
        rows.append({"arm": arm, "scenario": scenario, "service": "fail" if phase_failure else step.get("service_outcome"),
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
            "trace_contract": {"counter_totals": "exact", "phase_clock_tolerance_seconds": CLOCK_TOLERANCE_SECONDS},
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
