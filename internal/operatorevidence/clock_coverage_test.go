package operatorevidence

import (
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/pkg/callharness"
)

func TestCoverageWindowsPreservesEveryReceipt(t *testing.T) {
	at := time.Unix(1793930000, 123).UTC()
	a := callharness.OperationIdentity{RunID: "run", OperationID: "move", SessionID: "a", Worker: "w1", Epoch: 1}
	b := a
	b.SessionID = "b"
	pool := Pool{Phases: []MediaInput{{RawMedia: RawMedia{Events: []loadgen.EventResult{{Event: loadgen.Event{Kind: "move"}, Identities: []callharness.OperationIdentity{a, b}}}, Receipts: []operatorcaller.Receipt{{Identity: a, Kind: "move", Start: at, End: at.Add(time.Second)}, {Identity: b, Kind: "move", Start: at.Add(2 * time.Second), End: at.Add(3 * time.Second)}}}}}}
	windows := CoverageWindows(pool)
	if len(windows) != 2 || windows[0].StartUnixNS != "1793930000000000123" || windows[1].EndUnixNS != "1793930003000000123" {
		t.Fatal("receipt timestamps lost", windows)
	}
	pool.Phases[0].Receipts = pool.Phases[0].Receipts[:1]
	windows = CoverageWindows(pool)
	if len(windows) != 2 || windows[1].StartUnixNS != "" {
		t.Fatal("missing receipt silently omitted", windows)
	}
}
