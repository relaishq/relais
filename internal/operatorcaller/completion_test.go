package operatorcaller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type failedStopRead struct{ Store }

func (s failedStopRead) Read(ctx context.Context, key string) (ReadResult, error) {
	if strings.HasSuffix(key, "/stop.json") {
		return ReadResult{}, errors.New("stop-read-failed")
	}
	return s.Store.Read(ctx, key)
}

func TestValidAbortSurvivesReasonReadFailure(t *testing.T) {
	b, box, fence := completionBroker(t)
	put(t, box, b.Manifest.Job()+"/finished.json", map[string]any{"runId": b.Manifest.RunID, "job": "caller-1", "done": false, "stopped": true, "exitCode": 1, "sessions": []string{"one"}})
	b.Mailbox.Store = failedStopRead{box}
	raw, err := b.Collect(context.Background(), b.Clock.Now().Add(time.Minute))
	var stopped *StoppedError
	if !errors.As(err, &stopped) || stopped.Reason != "unavailable" || !strings.Contains(err.Error(), "stop-read-failed") || raw != "raw report" || b.collected {
		t.Fatalf("valid abort lost its terminal outcome: raw %q err %v", raw, err)
	}
	last := fence.rows[len(fence.rows)-1]
	if last.Rule != "caller-stopped" || last.Code != "unavailable" || b.Completion == nil || b.Completion.StopReason != "unavailable" {
		t.Fatalf("valid abort lost its retained cause: %+v completion %+v", last, b.Completion)
	}
}

func completionBroker(t *testing.T) (*Broker, *memoryBox, *recordedFence) {
	t.Helper()
	m := fixtureManifest()
	at := time.UnixMilli(m.CreatedAt)
	box := &memoryBox{}
	fence := &recordedFence{}
	b, err := NewBroker(m, NewMailbox(box), &testClock{at}, fence)
	if err != nil {
		t.Fatal(err)
	}
	b.Registration = &Registration{Sessions: []string{"one"}, Near: true}
	put(t, box, m.Job()+"/finished.json", map[string]any{"runId": m.RunID, "job": "caller-1", "done": true, "stopped": false, "exitCode": 0, "sessions": []string{"one"}})
	box.data[m.Job()+"/caller.jsonl"] = []byte("raw report")
	return b, box, fence
}

func TestCallerCompletionNormal(t *testing.T) {
	b, _, fence := completionBroker(t)
	raw, err := b.Collect(context.Background(), b.Clock.Now().Add(time.Minute))
	if err != nil || raw != "raw report" || !b.collected || fence.Latched() || len(fence.rows) != 0 {
		t.Fatalf("normal completion changed: raw %q err %v collected %t uncertainties %+v", raw, err, b.collected, fence.rows)
	}
}

func TestCallerCompletionMalformedReply(t *testing.T) {
	for name, receipt := range map[string]string{
		"invalid-json":         `{"runId":`,
		"wrong-run":            `{"runId":"run-other","job":"caller-1","done":true,"stopped":false,"exitCode":0,"sessions":["one"]}`,
		"missing-exit":         `{"runId":"run-fixture","job":"caller-1","done":true,"stopped":false,"sessions":["one"]}`,
		"not-done":             `{"runId":"run-fixture","job":"caller-1","done":false,"stopped":false,"exitCode":0,"sessions":["one"]}`,
		"aborted-wrong-run":    `{"runId":"run-other","job":"caller-1","done":false,"stopped":true,"exitCode":1,"sessions":["one"]}`,
		"aborted-missing-exit": `{"runId":"run-fixture","job":"caller-1","done":false,"stopped":true,"sessions":["one"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			b, box, fence := completionBroker(t)
			box.data[b.Manifest.Job()+"/finished.json"] = []byte(receipt)
			raw, err := b.Collect(context.Background(), b.Clock.Now().Add(time.Minute))
			if err == nil || !strings.Contains(err.Error(), "invalid-caller-receipt") || raw != "raw report" || b.collected {
				t.Fatalf("malformed completion accepted or evidence lost: raw %q err %v", raw, err)
			}
			if len(fence.rows) == 0 || fence.rows[0].Kind != "mailbox.read" || fence.rows[0].Rule != "malformed-reply" {
				t.Fatalf("malformed reply classification changed: %+v", fence.rows)
			}
		})
	}
}

func TestCallerStopRejectsMalformedReceipt(t *testing.T) {
	for name, receipt := range map[string]string{
		"invalid-json": `{"runId":`,
		"wrong-run":    `{"runId":"run-other","job":"caller-1","reason":"Generator saturation hard ceiling"}`,
		"wrong-job":    `{"runId":"run-fixture","job":"other","reason":"Generator saturation hard ceiling"}`,
		"missing-run":  `{"job":"caller-1","reason":"Generator saturation hard ceiling"}`,
	} {
		t.Run(name, func(t *testing.T) {
			b, box, fence := completionBroker(t)
			box.data[b.Manifest.Job()+"/stop.json"] = []byte(receipt)
			err := b.CheckStopped(context.Background())
			if err == nil || err.Error() != "invalid-caller-receipt" || b.Stop != nil || len(fence.rows) != 1 || fence.rows[0].Rule != "malformed-reply" {
				t.Fatalf("malformed stop accepted: err %v uncertainties %+v", err, fence.rows)
			}
		})
	}
}

func TestCallerStoppedReasonsAndReceiptOrder(t *testing.T) {
	for reason, want := range map[string]string{
		"Generator saturation hard ceiling":              "generator-saturation-hard-ceiling",
		"Generator saturation sustained wall-time":       "generator-saturation-sustained",
		"Generator saturation exceeds planned 1% budget": "generator-saturation-budget",
		"Invalid generator statistics":                   "invalid-generator-statistics",
		"Caller stream/evidence failure":                 "caller-stream-failure",
		"Caller supervision failure":                     "caller-supervision-failure",
		"private endpoint or token":                      "unavailable",
	} {
		for _, stopFirst := range []bool{false, true} {
			t.Run(want+map[bool]string{false: "/finished-first", true: "/stop-first"}[stopFirst], func(t *testing.T) {
				b, box, fence := completionBroker(t)
				key := b.Manifest.Job() + "/finished.json"
				put(t, box, key, map[string]any{"runId": b.Manifest.RunID, "job": "caller-1", "done": false, "stopped": true, "exitCode": 1, "sessions": []string{"one"}})
				put(t, box, b.Manifest.Job()+"/stop.json", map[string]string{"runId": b.Manifest.RunID, "job": "caller-1", "reason": reason})
				if stopFirst {
					finished := box.data[key]
					delete(box.data, key)
					box.onRead = func(read string) {
						if read == b.Manifest.Job()+"/stop.json" {
							box.data[key] = finished
						}
					}
				}
				raw, err := b.Collect(context.Background(), b.Clock.Now().Add(time.Minute))
				if err == nil || err.Error() != "caller-stopped:"+want || raw != "raw report" || b.collected || b.Completion == nil || b.Stop == nil {
					t.Fatalf("caller outcome or receipts lost: raw %q err %v completion %+v stop %+v", raw, err, b.Completion, b.Stop)
				}
				last := fence.rows[len(fence.rows)-1]
				if last.Rule != "caller-stopped" || last.Code != want || last.CallerID != b.Manifest.CallerID {
					t.Fatalf("fixed caller stop reason missing from report context: %+v", last)
				}
			})
		}
	}
}

func TestCallerStopDoesNotConfirmFeed(t *testing.T) {
	b, box, fence := completionBroker(t)
	put(t, box, b.Manifest.Job()+"/stop.json", map[string]string{"runId": b.Manifest.RunID, "job": "caller-1", "reason": "Generator saturation hard ceiling"})
	_, err := b.Feed(context.Background(), "test-operation", Payload{Kind: "finish"}, b.Clock.Now().Add(time.Minute))
	if err == nil || err.Error() != "command-result-unknown" || !fence.Latched() || fence.rows[len(fence.rows)-1].Rule != "caller-stopped" {
		t.Fatalf("caller stop confirmed an unacknowledged feed: err %v uncertainties %+v", err, fence.rows)
	}
}

func TestCallerAbortedCompletionRetainsEvidence(t *testing.T) {
	for _, done := range []bool{false, true} {
		t.Run(map[bool]string{false: "interrupted", true: "done"}[done], func(t *testing.T) {
			b, box, fence := completionBroker(t)
			put(t, box, b.Manifest.Job()+"/finished.json", map[string]any{"runId": b.Manifest.RunID, "job": "caller-1", "done": done, "stopped": true, "exitCode": 1, "sessions": []string{"one"}})
			put(t, box, b.Manifest.Job()+"/stop.json", map[string]string{"runId": b.Manifest.RunID, "job": "caller-1", "reason": "Generator saturation hard ceiling"})
			receipt := string(box.data[b.Manifest.Job()+"/finished.json"])
			raw, err := b.Collect(context.Background(), b.Clock.Now().Add(time.Minute))
			if err == nil || err.Error() != "caller-stopped:generator-saturation-hard-ceiling" || raw != "raw report" || b.collected {
				t.Fatalf("aborted completion misclassified: raw %q err %v collected %t", raw, err, b.collected)
			}
			if string(box.data[b.Manifest.Job()+"/finished.json"]) != receipt {
				t.Fatal("aborted completion receipt was lost")
			}
			if len(fence.rows) == 0 || fence.rows[len(fence.rows)-1].Rule != "caller-stopped" {
				t.Fatalf("aborted collection lost its cause: %+v", fence.rows)
			}
			for _, row := range fence.rows {
				if row.Rule == "malformed-reply" {
					t.Fatal("valid abort was marked malformed", row)
				}
			}
		})
	}
}
