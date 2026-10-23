#!/usr/bin/env python3
"""Summarize manual owner-approval evidence; never grant approval automatically."""
import argparse
import json
import math
import os
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument("report", type=Path)
parser.add_argument("--save-cpu-us", type=float, required=True)
parser.add_argument("--save-wall-us", type=float, required=True)
args = parser.parse_args()
for budget in (args.save_cpu_us, args.save_wall_us):
    if not math.isfinite(budget) or budget <= 0:
        parser.error("operator Save budgets must be finite positive microseconds/event")
data = json.loads(args.report.read_text())
if data.get("kind") == "split-capacity":
    from split_capacity_report import summarize
    summary = summarize(data, args.save_cpu_us, args.save_wall_us)
    text = json.dumps(summary, indent=2, allow_nan=False) + "\n"
    args.report.with_suffix(".approval.json").write_text(text)
    print(text, end="")
    if os.getenv("GITHUB_STEP_SUMMARY"):
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as output:
            output.write("Matched split-capacity: " + summary["approval"] + "\n\n" + "\n".join("- " + reason for reason in summary["blocking"]) + "\n")
    raise SystemExit(1 if summary["blocking"] else 0)

blocking = []
if data.get("outcome") != "pass" or not data.get("planned_load_validated"):
    blocking.append("capacity ramp / N+1 operating load is not validated")
if not data.get("custom_agent", {}).get("worker_sha256"):
    blocking.append("exact custom-worker build evidence is missing")
budgets = data.get("custom_agent", {}).get("operator_budgets")
if data.get("custom_agent", {}).get("operator_budget_error") or not isinstance(budgets, dict) or any(
        not isinstance(budgets.get(k), int) or isinstance(budgets.get(k), bool) or budgets[k] <= 0
        for k in ("max_snapshot_bytes", "snapshot_bytes_per_second", "snapshot_burst_bytes", "move_bytes_per_session", "move_bytes_total")):
    blocking.append("operator budgets unknown or invalid; provide workerapp-compatible budget evidence before approval")
steps = []
if not data.get("steps"):
    blocking.append("no capacity steps")
bounds = {kind: data.get(kind + "_capacity_lower_bound", 0) for kind in ("worker", "relay")}
for kind, bound in bounds.items():
    if bound <= 0 or not any(s["kind"] == kind and s["run"]["callers"] == bound for s in data.get("steps", [])):
        blocking.append(f"{kind}: validated lower bound proof is missing")
for kind in ("n-plus-one", "drain"):
    if not any(s["kind"] == kind and s["run"]["callers"] == data.get("candidate_n_plus_one_callers", 0)
               for s in data.get("steps", [])):
        blocking.append(f"{kind}: planned load proof is missing")
for step in data.get("steps", []):
    # A stopping ramp step measures the limit; it is not evidence at the
    # validated operating load. Preserve it verbatim, including its failures.
    required = step["kind"] not in bounds or step["run"]["callers"] <= bounds[step["kind"]]
    evidence = step.get("custom_agent", {})
    if required and (step.get("service_outcome") != "pass" or evidence.get("evidence_verdict") != "pass"):
        blocking.extend(step.get("causes", []) or ["incomplete custom-agent evidence"])
    for field, budget in (("save_cpu_seconds_per_event", args.save_cpu_us),
                          ("save_wall_seconds_per_event", args.save_wall_us)):
        value = evidence.get(field)
        if required and (value is None or not math.isfinite(value) or value < 0 or value * 1e6 > budget):
            blocking.append(f"{step['kind']}: {field} exceeds operator budget {budget} us/event or is missing")
    steps.append({"approval_scope": "required" if required else "raw-beyond-validated-bound",
                  "causes": step.get("causes", []),
                  "forced_writes": {"attempts": evidence.get("ForcedAttempts"),
                                    "successes": evidence.get("ForcedSuccesses"),
                                    "failures": evidence.get("ForcedFailures"),
                                    "interpretation": "ReadOnly delayed-loopback tasks require no forced intent writes; zero is expected for this workload. SideEffecting forced-write behavior is exercised by the ledger store-stall cut."},
                  "kind": step["kind"], "callers": step["run"]["callers"],
                  "service": step["service_outcome"], "custom_agent": evidence,
                  "store_commands_per_call_second": step["store_commands_per_call_second"],
                  "store_bytes_per_call_second": step["store_bytes_per_call_second"],
                  "store_bytes_by_client": step["store_bytes_by_client_per_call_second"],
                  "all_client_attempts_and_wire_rates": [
                      {"process": p["name"], "rates": {k: v for k, v in p.get("rates_per_call_second", {}).items()
                        if (k.startswith("relais_store_commands_total") or k.startswith("relais_store_wire_commands_total")) or ("relais_transport_bytes_total" in k and 'transport="redis"' in k)}}
                      for p in step["run"]["telemetry"]["processes"]],
                  "memory": [{"process": p["name"], "heap_peak": p["peak_heap_bytes"],
                              "rss_peak": p.get("peak_rss_bytes"), "rss_kind": p["rss_kind"]}
                             for p in step["run"]["telemetry"]["processes"]]})
summary = {"approval": "BLOCKING APPROVAL" if blocking else "EVIDENCE READY FOR OWNER REVIEW; approval is manual",
           "build": data.get("custom_agent"),
           "save_budgets_us_per_event": {"cpu": args.save_cpu_us, "wall": args.save_wall_us},
           "store_budgets_per_call_second": {"commands": data["store_commands_budget_per_call_second"],
                                             "request_reply_bytes": data["store_bytes_budget_per_call_second"]},
           "blocking": sorted(set(blocking)), "steps": steps}
text = json.dumps(summary, indent=2, allow_nan=False) + "\n"
args.report.with_suffix(".approval.json").write_text(text)
print(text, end="")
if os.getenv("GITHUB_STEP_SUMMARY"):
    with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as output:
        output.write("Manual custom-worker gate: " + summary["approval"] + "\n\n```json\n" + text + "```\n")
raise SystemExit(1 if blocking else 0)
