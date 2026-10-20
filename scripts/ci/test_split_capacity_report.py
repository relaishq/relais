import copy
import unittest

from split_capacity_report import CASES, summarize, valid_rejoin


def fixture():
    def phase():
        return dict(commands=100, request_bytes=10000, reply_bytes=1000,
                    components={"work": dict(writes=10, bytes=29010)},
                    key_arguments={f"{i:02d}": {} for i in range(1, 12)},
                    key_examples=["p:" + n + ":{sess:x}" for n in ("lease", "record", "epoch", "routes", "ended", "setup", "checkpoint")] + ["p:" + n + ":{sess:x}:" + str(b) for n in ("work", "history") for b in (0, 1)],
                    hset_field_values={"checkpoint:b": {}, "checkpoint:m": {},
                                       "checkpoint:t": dict(min_bytes=8, max_bytes=8)})
    steps = []
    for arm, scenario in CASES:
        custom = dict(evidence_verdict="pass", checkpoint_age_p99_ms=100, snapshot_age_p99_ms=110,
                      checkpoint_age_max_ms=150, snapshot_age_max_ms=160,
                      save_cpu_seconds_per_event=1e-6, save_wall_seconds_per_event=2e-6,
                      admission_calls=100, admission_cpu_unavailable=0, admission_cpu_seconds=.01,
                      admission_wall_seconds=.02, checkpoint_skips={"budget": 0, "interval": 0},
                      ForcedSuccesses=2, ForcedFailures=0, forced_max_ms=5)
        measurement = dict(worker_args=["-agent-split-checkpoints"] if arm == "after" else ([] if scenario == "echo" else ["-agent-max-snapshot-bytes=24576", "-agent-snapshot-bytes-per-second=270336", "-agent-snapshot-burst-bytes=98304"]), ages=dict(coverage_complete=True, checkpoint_age_p99_ms=100, snapshot_age_p99_ms=100, checkpoint_age_max_ms=150, snapshot_age_max_ms=150), arm=arm, scenario=scenario, live_calls=2, lost_calls=0,
                           survivors={"1": 1, "2": 1}, fixture_samples=100, history_refusals=10,
                           inventory_live_calls=2, live_key_id_inventory={"2": 10},
                           store=dict(phases={p: phase() for p in ("setup", "steady")},
                                      phase_seconds={p: 20 for p in ("setup", "steady")},
                                      seconds={"0": phase()}))
        if scenario == "rotation":
            measurement["store"]["phases"]["rotation"] = phase()
            measurement["store"]["phase_seconds"]["rotation"] = 11
        if scenario == "history-churn":
            measurement["store"]["phase_seconds"]["steady"] = 9
            measurement["store"]["phases"]["churn"] = phase()
            measurement["store"]["phase_seconds"]["churn"] = 11
        steps.append(dict(service_outcome="pass", generator_outcome="valid", measurement=measurement,
                          custom_agent=custom, store_commands_per_call_second=70, store_bytes_per_call_second=100000,
                          relay_buffer_enabled=True,
                          run=dict(callers=2, duration_ns=20000000000, leaks=dict(checked=True),
                                   worker_rejoins=[dict(started_at="2026-10-20T00:00:00Z", completed_at="2026-10-20T00:00:02Z", last_heartbeat="2026-10-20T00:00:01Z", old_pid=1, pid=2)] * 3)))
    return dict(kind="split-capacity", outcome="complete", goos="linux", runner="one-runner", callers=2, duration_seconds=20,
                binary_sha256={n: "a" * 64 for n in ("relais-load", "relais-worker", "delayed-loopback", "relais-relay", "relais-control")},
                store_commands_budget_per_call_second=100, store_bytes_budget_per_call_second=131072, steps=steps)


class SplitCapacityReport(unittest.TestCase):
    def test_later_churn_phase_cannot_hide_behind_passing_window(self):
        for field, value in (("request_bytes", 132000 * 2 * 11),
                             ("commands", 101 * 2 * 11)):
            with self.subTest(field=field):
                data = fixture()
                step = data["steps"][4]
                trace = step["measurement"]["store"]
                trace["phases"]["churn"].update(reply_bytes=0, **{field: value})
                summary = summarize(data, 10, 20)
                row = summary["steps"][4]
                self.assertLess(row["observed_media_bytes_per_call_second"], 131072)
                self.assertLess(row["observed_media_commands_per_call_second"], 100)
                self.assertLess(row["lifecycle_bytes_per_media_call_second"], 131072)
                self.assertIn("after/history-churn: churn attempts exceed store ceiling", summary["blocking"])
                self.assertEqual("fail", row["service"])

    def test_missing_churn_phase_blocks_evidence(self):
        data = fixture()
        del data["steps"][4]["measurement"]["store"]["phases"]["churn"]
        self.assertIn("after/history-churn: churn traffic window missing", summarize(data, 10, 20)["blocking"])

    def test_truncated_churn_phase_blocks_evidence(self):
        data = fixture()
        data["steps"][4]["measurement"]["store"]["phase_seconds"]["churn"] = .1
        self.assertIn("after/history-churn: churn traffic window too short", summarize(data, 10, 20)["blocking"])

    def test_complete_evidence_and_actual_rates(self):
        summary = summarize(fixture(), 10, 20)
        self.assertEqual([], summary["blocking"])
        self.assertEqual(275, summary["steps"][1]["phases"]["steady"]["request_reply_bytes_per_call_second"])
        self.assertEqual(.25, summary["steps"][1]["phases"]["steady"]["component_writes_per_call_second"]["work"])

    def test_expected_legacy_byte_failure_is_preserved(self):
        data = fixture()
        data["steps"][0].update(service_outcome="fail", store_bytes_per_call_second=187000,
                                causes=["store budget exceeded; checkpoint cadence requires follow-up before operating-load approval"])
        summary = summarize(data, 10, 20)
        self.assertEqual([], summary["blocking"])
        self.assertTrue(summary["steps"][0]["baseline_over_budget"])
        data["steps"][0]["causes"].append("decryption failure")
        self.assertTrue(summarize(data, 10, 20)["blocking"])

    def test_missing_or_invalid_evidence_blocks(self):
        changes = [
            lambda d: d.update(goos="darwin"),
            lambda d: d["steps"][1]["measurement"]["store"]["phases"]["setup"].update(request_bytes=6000000),
            lambda d: d["steps"].pop(),
            lambda d: d["steps"][1]["run"].update(duration_ns=30000000000),
            lambda d: d["steps"][1].update(relay_buffer_enabled=False),
            lambda d: d["steps"][1].update(generator_outcome="inconclusive"),
            lambda d: d["steps"][1].update(store_bytes_per_call_second=131073),
            lambda d: d["steps"][1]["measurement"].update(live_calls=1),
            lambda d: d["steps"][1]["run"]["leaks"].update(checked=False),
            lambda d: d["steps"][1]["custom_agent"].update(snapshot_age_p99_ms=551),
            lambda d: d["steps"][1]["custom_agent"].update(checkpoint_age_max_ms=None),
            lambda d: d["steps"][1]["custom_agent"].update(admission_calls=0),
            lambda d: d["steps"][1]["custom_agent"].update(admission_cpu_unavailable=1),
            lambda d: d["steps"][1]["custom_agent"].update(ForcedSuccesses=0),
            lambda d: d["steps"][1]["measurement"]["store"]["phases"]["steady"].update(key_arguments={}),
            lambda d: d["steps"][1]["measurement"]["store"]["phases"]["steady"].update(hset_field_values={}),
            lambda d: d["steps"][1]["measurement"]["store"].update(errors=["truncated reply"]),
            lambda d: d["steps"][4]["measurement"].update(history_refusals=0),
            lambda d: d["steps"][5]["measurement"].update(live_key_id_inventory={"1": 1, "2": 9}),
            lambda d: d["steps"][5]["run"].update(worker_rejoins=[]),
            lambda d: d["steps"][6]["measurement"].update(survivors={"0": 1, "1": 1}),
        ]
        for i, change in enumerate(changes):
            with self.subTest(i=i):
                data = copy.deepcopy(fixture())
                change(data)
                self.assertTrue(summarize(data, 10, 20)["blocking"])


class RejoinTimestamp(unittest.TestCase):
    def test_variable_width_go_timestamps(self):
        worker = dict(started_at="2026-10-20T07:22:56.329489123-07:00", completed_at="2026-10-20T07:22:56.35634-07:00", last_heartbeat="2026-10-20T07:22:56.346042-07:00", pid=2, old_pid=1)
        self.assertTrue(valid_rejoin(worker))
        worker["completed_at"] = "0001-01-01T00:00:00Z"
        self.assertFalse(valid_rejoin(worker))
