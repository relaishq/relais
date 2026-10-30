package agentbridge

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBridgeRegistryLateRepliesDoNotCloseIdleRunner(t *testing.T) {
	r := &runner{replies: make(chan reservationReply, 1)}
	// Both replies can arrive after the canceled caller stopped receiving.
	require.NoError(t, r.receiveReply(reservationReply{Type: "reserved", ID: "expired-1", Accepted: true}))
	require.NoError(t, r.receiveReply(reservationReply{Type: "reserved", ID: "expired-2", Accepted: true}), "two valid late replies must not close the runner")
	require.Empty(t, r.replies, "no request is waiting for these replies")
}

func TestBridgeRegistryRepliesStayWithCurrentReservation(t *testing.T) {
	r := &runner{replies: make(chan reservationReply, 1)}
	r.setReservation("old")
	require.NoError(t, r.receiveReply(reservationReply{Type: "reserved", ID: "old", Accepted: true}))
	r.setReservation("current")
	require.Empty(t, r.replies, "switching requests retires the predecessor's cached reply")
	for i := 0; i < 3; i++ {
		require.NoError(t, r.receiveReply(reservationReply{Type: "reserved", ID: "old", Accepted: true}))
	}
	require.Empty(t, r.replies)
	first := reservationReply{Type: "reserved", ID: "current", Accepted: true}
	require.NoError(t, r.receiveReply(first))
	require.NoError(t, r.receiveReply(first))
	require.Len(t, r.replies, 1, "duplicate acknowledgements do not overflow the mailbox")
	require.Equal(t, first, <-r.replies)
	r.setReservation("")
	require.NoError(t, r.receiveReply(first))
	require.Empty(t, r.replies, "responses after cancellation cannot poison the next RPC")
	require.ErrorIs(t, r.receiveReply(reservationReply{Type: "unexpected", ID: "current"}), errProtocol)
}
