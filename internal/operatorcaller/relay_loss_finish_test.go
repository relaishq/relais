package operatorcaller

import (
	"context"
	"testing"
)

func TestRelayLossFinishDoesNotArmOrAbortCaller(t *testing.T) {
	calls := 0
	ack, err := Deliver(context.Background(), Feed{RunID: "r", OperationID: "relay-loss-finish", Payload: Payload{Kind: "relay-loss-finish"}}, nil, func(_ context.Context, path string, _, _ any) error {
		calls++
		if path != "/relay-loss-finish" {
			t.Fatal(path)
		}
		return nil
	}, func() { t.Fatal("terminal abort") }, SystemClock{})
	if err != nil || !ack.OK || calls != 1 {
		t.Fatal(ack, err, calls)
	}
}
