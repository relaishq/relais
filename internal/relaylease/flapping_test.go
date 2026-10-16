package relaylease

import (
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
)

// Replay seeded poll failures through the production observer using its default
// TTL/poll cadence and monotonic time. This measures observation-to-claim
// eligibility, excluding kernel fencing, activation and socket startup.
func TestStalenessSeededFlapping(t *testing.T) {
	for _, rate := range []int{30, 40, 50} {
		var times []time.Duration
		timeouts := 0
		for seed := int64(0); seed < 15; seed++ {
			rng := rand.New(rand.NewSource(seed))
			now := time.Now()
			lease := sessionstore.RelayLease{Holder: process("frozen"), Epoch: 1, ExpiresAt: now.Add(-DefaultTTL), Expired: true}
			var observer staleness
			finished := false
			for elapsed := time.Duration(0); elapsed <= 60*time.Second; elapsed += DefaultPoll {
				var err error
				if rng.Intn(100) < rate {
					err = sessionstore.ErrTransient
				}
				if observer.observe(lease, false, err, now.Add(elapsed), DefaultTTL, DefaultRenew) {
					times = append(times, elapsed)
					finished = true
					break
				}
			}
			if !finished {
				timeouts++
			}
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		if len(times) == 0 {
			t.Fatalf("all seeds timed out at %d%%", rate)
		}
		if timeouts != 0 {
			t.Errorf("%d%% failures: %d seeds exceeded 60s", rate, timeouts)
		}
		t.Logf("FLAPPING rate=%d%% seeds=15 median=%s max=%s timeouts=%d/15 limit=60s", rate, times[len(times)/2], times[len(times)-1], timeouts)
	}
}
