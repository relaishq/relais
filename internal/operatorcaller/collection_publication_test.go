package operatorcaller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

func pendingStoppedCaller(t *testing.T) (*Broker, *memoryBox, []byte) {
	t.Helper()
	b, box, _ := completionBroker(t)
	key := b.Manifest.Job() + "/finished.json"
	put(t, box, key, map[string]any{"runId": b.Manifest.RunID, "job": "caller-1", "done": false, "stopped": true, "exitCode": 1, "sessions": []string{"one"}})
	finished := box.data[key]
	delete(box.data, key)
	delete(box.data, b.Manifest.Job()+"/caller.jsonl")
	put(t, box, b.Manifest.Job()+"/stop.json", map[string]string{"runId": b.Manifest.RunID, "job": "caller-1", "reason": "Generator saturation hard ceiling"})
	return b, box, finished
}

func TestStoppedCallerMissingEvidenceStaysStopped(t *testing.T) {
	b, _, _ := pendingStoppedCaller(t)
	raw, err := b.Collect(context.Background(), b.Clock.Now().Add(5*time.Second))
	var stopped *StoppedError
	if !errors.As(err, &stopped) || stopped.Reason != "generator-saturation-hard-ceiling" || raw != "" || b.Completion != nil || b.collected || !strings.Contains(err.Error(), "missing-raw-caller") {
		t.Fatalf("missing evidence upgraded or erased the stopped outcome: raw %q err %v completion %+v", raw, err, b.Completion)
	}
}

type publicationClock struct {
	testClock
	publish func(time.Time)
}

func (c *publicationClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := c.testClock.Sleep(ctx, d); err != nil {
		return err
	}
	c.publish(c.Now())
	return nil
}

func TestStoppedCallerWaitsForDelayedPublication(t *testing.T) {
	b, box, finished := pendingStoppedCaller(t)
	start := b.Clock.Now()
	b.Clock = &publicationClock{testClock: testClock{start}, publish: func(now time.Time) {
		if now.Sub(start) >= 1500*time.Millisecond {
			box.data[b.Manifest.Job()+"/caller.jsonl"] = []byte("delayed report")
		}
		if now.Sub(start) >= 2500*time.Millisecond {
			box.data[b.Manifest.Job()+"/finished.json"] = finished
		}
	}}
	raw, err := b.Collect(context.Background(), start.Add(5*time.Second))
	var stopped *StoppedError
	if !errors.As(err, &stopped) || stopped.Reason != "generator-saturation-hard-ceiling" || raw != "delayed report" || b.Completion == nil || !b.Completion.Stopped || b.collected || b.Clock.Now().Sub(start) != 2500*time.Millisecond {
		t.Fatalf("delayed stopped caller evidence lost: raw %q err %v completion %+v elapsed %v", raw, err, b.Completion, b.Clock.Now().Sub(start))
	}
}

func TestStoppedCallerPublicationWaitUsesExistingEnd(t *testing.T) {
	for _, manifestFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "gate-end", true: "manifest-end"}[manifestFirst], func(t *testing.T) {
			b, _, _ := pendingStoppedCaller(t)
			end := b.Clock.Now().Add(2 * time.Second)
			want := end
			if manifestFirst {
				want = b.Clock.Now().Add(time.Second)
				b.Manifest.Deadline = want.UnixMilli()
			}
			_, err := b.Collect(context.Background(), end)
			var stopped *StoppedError
			if !errors.As(err, &stopped) || b.Completion != nil || b.collected || !b.Clock.Now().Equal(want) {
				t.Fatalf("stopped publication wait changed its bound or certainty: err %v completion %+v now %v want %v", err, b.Completion, b.Clock.Now(), want)
			}
		})
	}
}

func protocolStoppedCaller(t *testing.T, failReceiptAt int) *Broker {
	t.Helper()
	b, box, finished := pendingStoppedCaller(t)
	box.data[b.Manifest.Job()+"/caller.jsonl"] = []byte("protocol report")
	reads := 0
	peer := &mailboxPeer{reply: func(f operator.Frame) (operator.Frame, error) {
		var args MailboxArgs
		if err := json.Unmarshal(f.Request.Args, &args); err != nil {
			t.Fatal(err)
		}
		data, found := box.data[args.Key]
		if strings.HasSuffix(args.Key, "/finished.json") {
			reads++
			if reads == failReceiptAt {
				return operator.Frame{}, errors.New("receipt-read-failed")
			}
			if reads > 1 {
				data, found = finished, true
			}
		}
		payload, err := json.Marshal(MailboxValue{Found: &found, Data: data})
		if err != nil {
			t.Fatal(err)
		}
		return operator.Frame{Type: "result", Session: f.Session, Result: &operator.Reply{ID: f.Request.ID, Transport: operator.Succeeded, ExitCode: new(int), Payload: payload}}, nil
	}}
	b.Mailbox = NewMailbox(&ProtocolMailbox{Peer: peer, Session: "caller", End: time.Now().Add(time.Minute)})
	return b
}

func TestStoppedCallerProtocolReportAvailable(t *testing.T) {
	b := protocolStoppedCaller(t, 0)
	raw, err := b.Collect(context.Background(), b.Clock.Now().Add(time.Minute))
	var stopped *StoppedError
	if !errors.As(err, &stopped) || raw != "protocol report" || b.Completion == nil || b.collected {
		t.Fatalf("protocol report or stopped outcome lost: raw %q err %v", raw, err)
	}
}

func TestStoppedCallerReceiptFailureRetainsRawReport(t *testing.T) {
	b := protocolStoppedCaller(t, 2)
	raw, err := b.Collect(context.Background(), b.Clock.Now().Add(time.Minute))
	var stopped *StoppedError
	if !errors.As(err, &stopped) || raw != "protocol report" || b.Completion != nil || b.collected || !strings.Contains(err.Error(), "mailbox-result-unknown") {
		t.Fatalf("receipt read failure erased raw report: raw %q err %v completion %+v", raw, err, b.Completion)
	}
}

func TestKnownStopProtocolReportAvailable(t *testing.T) {
	b := protocolStoppedCaller(t, 0)
	var stopped *StoppedError
	if err := b.CheckStopped(context.Background()); !errors.As(err, &stopped) {
		t.Fatal("stop was not observed before collection", err)
	}
	raw, err := b.Collect(context.Background(), b.Clock.Now().Add(time.Minute))
	if !errors.As(err, &stopped) || raw != "protocol report" || b.Completion == nil || b.collected {
		t.Fatalf("known stopped caller lost its report or outcome: raw %q err %v", raw, err)
	}
}

func TestKnownStopReceiptFailureRetainsRawReport(t *testing.T) {
	b := protocolStoppedCaller(t, 1)
	var stopped *StoppedError
	if err := b.CheckStopped(context.Background()); !errors.As(err, &stopped) {
		t.Fatal("stop was not observed before collection", err)
	}
	raw, err := b.Collect(context.Background(), b.Clock.Now().Add(time.Minute))
	if !errors.As(err, &stopped) || raw != "protocol report" || b.Completion != nil || b.collected || !strings.Contains(err.Error(), "mailbox-result-unknown") {
		t.Fatalf("known stop did not protect raw report from receipt failure: raw %q err %v", raw, err)
	}
}
