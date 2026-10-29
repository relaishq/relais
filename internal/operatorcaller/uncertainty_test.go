package operatorcaller

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/operator"
)

type recordedFence struct {
	testFence
	rows []operator.Uncertainty
}

func (f *recordedFence) RecordUncertainty(u operator.Uncertainty) {
	f.rows = append(f.rows, u.Public())
}

func TestRegistrationAckNeedsMoreThanOneMinute(t *testing.T) {
	for _, seconds := range []int{60, 120} {
		t.Run(strconv.Itoa(seconds), func(t *testing.T) {
			m := fixtureManifest()
			clock := &testClock{time.UnixMilli(m.CreatedAt)}
			start := clock.Now()
			box := &memoryBox{}
			box.onRead = func(key string) {
				// The unchanged loader has one startup probe, then one every 10s.
				if strings.Contains(key, "/acks/") && clock.Now().Sub(start) >= 90*time.Second {
					put(t, box, key, Ack{RunID: m.RunID, Job: "caller-1", Sequence: 0, OperationID: "caller-probes", OK: true})
				}
			}
			f := &recordedFence{}
			b, err := NewBroker(m, NewMailbox(box), clock, f)
			if err != nil {
				t.Fatal(err)
			}
			_, err = b.Feed(context.Background(), "caller-probes", Payload{Kind: "probe", Count: ClockBurstCount, Deadline: time.UnixMilli(m.Deadline)}, start.Add(time.Duration(seconds)*time.Second))
			if seconds == 120 {
				if err != nil || f.Latched() || clock.Now().Sub(start) != 90*time.Second {
					t.Fatal("registration did not accept the tenth probe", err, f.rows)
				}
			} else {
				if err == nil || err.Error() != "command-result-unknown" || !f.Latched() {
					t.Fatal("short registration wait did not reproduce unknown", err)
				}
				last := f.rows[len(f.rows)-1]
				if last.Kind != "caller.feed" || last.RequestID != "0" || last.CallerID != m.CallerID || last.Action != "caller-probes" || last.Rule != "timeout" || last.RemainingMS != 0 || last.DeadlineUnixMS == "" {
					t.Fatal("unknown lost its request identity or certainty rule", last)
				}
			}
		})
	}
}

func TestCollectProbesUsesUnchangedLoaderCadence(t *testing.T) {
	start := time.Now().Add(time.Hour)
	clock := &testClock{start}
	burst, err := CollectProbes(context.Background(), ClockBurstCount, start.Add(2*time.Minute), func(_ context.Context, _ string, _, value any) error {
		n := min(ClockBurstCount, 1+int(clock.Now().Sub(start)/(10*time.Second)))
		probes := make([]callharness.ClockProbe, n)
		for i := range probes {
			at := start.Add(time.Duration(i) * 10 * time.Second)
			probes[i] = callharness.ClockProbe{CallerSend: at, CallerReceive: at.Add(time.Millisecond), ServerReceive: at.Add(time.Microsecond), ServerSend: at.Add(2 * time.Microsecond)}
		}
		*value.(*[]callharness.ClockProbe) = probes
		return nil
	}, clock)
	if err != nil || len(burst.Probes) != 10 || clock.Now().Sub(start) != 90*time.Second {
		t.Fatal("clock registration cadence changed", burst, err)
	}
}
