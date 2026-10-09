package callharness_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/relais/pkg/callharness"
)

// TestDialRacingCloseFailsCleanly dials several calls while the harness
// closes. Each Dial either connects (and Close then closes the call) or
// fails with an error; nothing panics, leaks or races. A Dial after Close
// fails with ErrHarnessClosed.
func TestDialRacingCloseFailsCleanly(t *testing.T) {
	h, err := callharness.Start(callharness.Options{})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const dials = 4
	var wg sync.WaitGroup
	errs := make([]error, dials)
	for i := range dials {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 50 * time.Millisecond) // some before Close, some during
			_, errs[i] = h.Dial(ctx, callharness.CallOptions{})
		}()
	}
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, h.Close())
	wg.Wait()

	for i, err := range errs {
		t.Logf("dial %d: %v", i, err)
	}

	_, err = h.Dial(ctx, callharness.CallOptions{})
	assert.ErrorIs(t, err, callharness.ErrHarnessClosed, "dial after Close")
}
