import copy
import unittest

from split_capacity_report import CASES, summarize, valid_rejoin, rolling_phase


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
                                      seconds={str(i): phase() for i in range(61)},
                                      phase_intervals={"setup": [dict(start=0, end=20)], "steady": [dict(start=20, end=40)]}))
        cap, rate, burst = (6144, 70000, 40960) if arm == "after" else ((7168, 81920, 28672) if scenario == "echo" else (24576, 270336, 98304))
        measurement["operator_budgets"] = dict(max_snapshot_bytes=cap, snapshot_bytes_per_second=rate,
            snapshot_burst_bytes=burst, move_bytes_per_session=1048576, move_bytes_total=67108864,
            checkpoint_age_p99_ms=550, snapshot_age_p99_ms=550)
        if scenario != "echo":
            measurement["fixture_environment"] = {"RELAIS_HISTORY_FULL_SIZE": "1"}
            measurement["fixture_sizes"] = {str(i): dict(working_bytes=3072, history_bytes=8192,
                restored_working_bytes=3072, restored_history_bytes=8192) for i in range(2)}
        if scenario == "rotation":
            measurement["store"]["phases"]["rotation"] = phase()
            measurement["store"]["phase_seconds"]["rotation"] = 11
            measurement["store"]["phase_intervals"]["rotation"] = [dict(start=40, end=51)]
        if scenario == "history-churn":
            measurement["store"]["phase_seconds"]["steady"] = 9
            measurement["store"]["phases"]["churn"] = phase()
            measurement["store"]["phase_seconds"]["churn"] = 11
            measurement["store"]["phase_intervals"]["steady"] = [dict(start=20, end=29)]
            measurement["store"]["phase_intervals"]["churn"] = [dict(start=29, end=40)]
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

    def test_sustained_failure_in_every_split_phase_blocks_passing_average(self):
        for index, phase_name in ((1, "steady"), (3, "steady"), (4, "churn"), (5, "rotation")):
            with self.subTest(index=index, phase=phase_name):
                data = fixture()
                trace = data["steps"][index]["measurement"]["store"]
                start = int(trace["phase_intervals"][phase_name][0]["start"])
                for i in range(start, start+10):
                    trace["seconds"][str(i)].update(request_bytes=2*131073, reply_bytes=0)
                summary = summarize(data, 10, 20)
                self.assertIn(f"after/{CASES[index][1]}: {phase_name} sustained 10-second attempts exceed store ceiling", summary["blocking"])
                self.assertEqual("fail", summary["steps"][index]["service"])
                self.assertLess(summary["steps"][index]["observed_media_bytes_per_call_second"], 131072)

    def test_missing_full_size_or_budget_evidence_blocks(self):
        for change in (
            lambda m: m.pop("fixture_environment"),
            lambda m: m["fixture_sizes"].pop("0"),
            lambda m: m["fixture_sizes"]["0"].update(restored_history_bytes=4205),
            lambda m: m["fixture_sizes"]["0"].update(working_bytes=2871),
            lambda m: m["operator_budgets"].update(max_snapshot_bytes=7168),
        ):
            data = fixture()
            change(data["steps"][1]["measurement"])
            self.assertTrue(summarize(data, 10, 20)["blocking"])

    def test_restore_coverage_matches_the_actual_event(self):
        data = fixture()
        for index in (1, 4):
            data["steps"][index]["measurement"]["fixture_sizes"]["0"].update(restored_working_bytes=0, restored_history_bytes=0)
        self.assertEqual([], summarize(data, 10, 20)["blocking"])
        data["steps"][1]["measurement"]["fixture_sizes"]["1"].update(restored_working_bytes=0, restored_history_bytes=0)
        self.assertTrue(summarize(data, 10, 20)["blocking"])
        for index in (5, 6, 7):
            data = fixture()
            data["steps"][index]["measurement"]["fixture_sizes"]["0"].update(restored_working_bytes=0, restored_history_bytes=0)
            self.assertTrue(summarize(data, 10, 20)["blocking"])

    def test_churn_requires_ten_contiguous_full_bins(self):
        data = fixture()
        trace = data["steps"][4]["measurement"]["store"]
        trace["phase_intervals"]["churn"] = [dict(start=29.1, end=39.9)]
        trace["phase_seconds"]["churn"] = 10.8
        self.assertIn("after/history-churn: churn has fewer than 10 contiguous full bins", summarize(data, 10, 20)["blocking"])

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


class RollingPhase(unittest.TestCase):
    def trace(self, values, intervals=None):
        intervals = intervals or [dict(start=10, end=31)]
        return dict(seconds={str(i+10): dict(request_bytes=v, reply_bytes=0) for i, v in enumerate(values)},
                    phase_intervals={"churn": intervals}, phase_seconds={"churn": sum(i["end"]-i["start"] for i in intervals)})

    def test_historical_churn_and_call_denominator(self):
        values = [275895,258169,266208,262642,278461,259803,273118,241214,271747,258710,276339,262307,280478,244848,266156,239893,306885,261181,249893,251886,272772]
        self.assertAlmostEqual(132347.7380952381, sum(values)/42)
        result = rolling_phase(self.trace(values), "churn", 2)
        self.assertEqual(21, result["full_bins"])
        self.assertEqual(12, result["window_count"])
        self.assertEqual(dict(first_bin=18, last_bin=27, request_reply_bytes_per_call_second=133427.2), result["worst"])

    def test_exact_ceiling_and_fractional_edges(self):
        trace = self.trace([2*131072]*21, [dict(start=10.1, end=30.9)])
        trace["seconds"]["10"]["request_bytes"] = 1e9
        trace["seconds"]["30"]["request_bytes"] = 1e9
        result = rolling_phase(trace, "churn", 2)
        self.assertEqual(19, result["full_bins"])
        self.assertEqual(131072, result["worst"]["request_reply_bytes_per_call_second"])
        data = fixture()
        for step in data["steps"]:
            for value in step["measurement"]["store"]["seconds"].values():
                value.update(request_bytes=2*131072, reply_bytes=0)
        self.assertEqual([], summarize(data, 10, 20)["blocking"])

    def test_separate_short_intervals_do_not_form_one_window(self):
        result = rolling_phase(self.trace([1]*21, [dict(start=10, end=16), dict(start=20, end=26)]), "churn", 2)
        self.assertEqual(12, result["full_bins"])
        self.assertIsNone(result["worst"])

    def test_invalid_evidence_is_not_zero_traffic(self):
        changes = [lambda t: t["seconds"].pop("15"),
                   lambda t: t["seconds"]["15"].update(request_bytes=float("nan")),
                   lambda t: t["seconds"]["15"].update(reply_bytes=-1),
                   lambda t: t["seconds"]["15"].update(reply_bytes=True),
                   lambda t: t.pop("phase_intervals"),
                   lambda t: t["phase_intervals"]["churn"][0].update(end=float("inf")),
                   lambda t: t["phase_seconds"].update(churn=22),
                   lambda t: t["phase_intervals"]["churn"].append(dict(start=20, end=31))]
        for change in changes:
            with self.subTest(change=change):
                trace = self.trace([1]*21)
                change(trace)
                with self.assertRaises(ValueError):
                    rolling_phase(trace, "churn", 2)
        for calls in (0, True, float("inf")):
            with self.assertRaises(ValueError):
                rolling_phase(self.trace([1]*21), "churn", calls)


class RejoinTimestamp(unittest.TestCase):
    def test_variable_width_go_timestamps(self):
        worker = dict(started_at="2026-10-20T07:22:56.329489123-07:00", completed_at="2026-10-20T07:22:56.35634-07:00", last_heartbeat="2026-10-20T07:22:56.346042-07:00", pid=2, old_pid=1)
        self.assertTrue(valid_rejoin(worker))
        worker["completed_at"] = "0001-01-01T00:00:00Z"
        self.assertFalse(valid_rejoin(worker))
