package sessionstore

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	codec "github.com/relaishq/relais/internal/componentcodec"
	"github.com/stretchr/testify/require"
)

func TestComponentFinalizerPublishesExactSelectedCharge(t *testing.T) {
	forComponentStores(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		lease, err := s.Claim(ctx, "finalize", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
		require.NoError(t, err)
		cs := s.(ComponentStore)
		parts := componentFixture(lease.SessionID)
		charge := 0
		finalize := func(n int) ([]byte, error) {
			charge = n
			var transport codec.TransportState
			require.NoError(t, codec.Unmarshal(parts.Transport, codec.Transport, &transport))
			transport.AgentRevision = uint64(n)
			return codec.Marshal(codec.Transport, transport)
		}
		first, err := cs.CommitComponents(ctx, lease, ComponentCommit{Components: parts, FinalizeTransport: finalize})
		require.NoError(t, err)
		require.Equal(t, first.WrittenBytes, charge)
		read, err := cs.ReadComponents(ctx, lease.SessionID)
		require.NoError(t, err)
		var transport codec.TransportState
		require.NoError(t, codec.Unmarshal(read.Components.Transport, codec.Transport, &transport))
		require.EqualValues(t, first.WrittenBytes, transport.AgentRevision)
		require.Equal(t, first.Components, read.Components)
		before := storedComponentRecord(t, s, lease.SessionID)
		for _, f := range []func(int) ([]byte, error){
			func(int) ([]byte, error) { return append(parts.Transport, 0), nil },
			func(int) ([]byte, error) { return nil, errors.New("capture refused") },
		} {
			_, err = cs.CommitComponents(ctx, lease, ComponentCommit{Components: read.Components, Expected: &first.Metadata, FinalizeTransport: f})
			require.Error(t, err)
			require.Equal(t, before, storedComponentRecord(t, s, lease.SessionID), "refusal precedes every component write")
		}
	})
}
