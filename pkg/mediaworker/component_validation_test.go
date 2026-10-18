package mediaworker

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestComponentContentDigestsDifferAcrossSessions(t *testing.T) {
	master := bytes.Repeat([]byte{0x53}, 32)
	snap := componentFixture(t)
	c, err := splitSessionComponents(snap, []byte("identical history"), nil)
	require.NoError(t, err)
	m := componentMaxMetadata()
	sealedA, a, err := sealSessionComponents(master, 255, "session-a", m, c)
	require.NoError(t, err)
	_, b, err := sealSessionComponents(master, 255, "session-b", m, c)
	require.NoError(t, err)
	for i := range a.References {
		require.NotEqual(t, a.References[i].Digest, b.References[i].Digest, "reference %d must not link equal content across sessions", i)
	}
	_, err = openSessionComponents(map[uint8][]byte{255: master}, "session-b", a, sealedA)
	require.Error(t, err)
	plain, err := openSessionComponents(map[uint8][]byte{255: master}, "session-a", a, sealedA)
	require.NoError(t, err)
	require.Equal(t, c, plain)
	transferred := a
	transferred.Owner = "new-worker"
	transferred.OwnerEpoch--
	for _, part := range []struct {
		kind byte
		data []byte
	}{{componentSetup, sealedA.Setup}, {componentWork, sealedA.Work}, {componentHistory, sealedA.History}} {
		_, err = openComponent(master, 255, "session-a", part.kind, transferred, part.data)
		require.NoError(t, err, "Transfer must retain the same session keys")
	}
}

func TestComponentAssemblyRefusesSemanticAliases(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*connectionSetupComponent, *transportComponent)
	}{
		{"null-host", func(_ *connectionSetupComponent, tr *transportComponent) {
			tr.LegacyVersion = 7
			tr.Host = []byte("null")
		}},
		{"mapped-ipv4", func(_ *connectionSetupComponent, tr *transportComponent) { tr.RemoteAddr = "[::ffff:127.0.0.1]:53458" }},
		{"expanded-ipv6", func(_ *connectionSetupComponent, tr *transportComponent) {
			tr.RemoteAddr = "[0:0:0:0:0:ffff:7f00:1]:53458"
		}},
		{"padded-port", func(_ *connectionSetupComponent, tr *transportComponent) { tr.RemoteAddr = "127.0.0.1:053458" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := mutatedComponents(t, tc.mutate)
			_, _, err := assembleSessionComponents(c, 1)
			require.ErrorIs(t, err, errBadState)
		})
	}
}

func mutatedComponents(t *testing.T, mutate func(*connectionSetupComponent, *transportComponent)) sessionComponents {
	t.Helper()
	c, err := splitSessionComponents(componentFixture(t), nil, nil)
	require.NoError(t, err)
	var setup connectionSetupComponent
	var transport transportComponent
	require.NoError(t, unmarshalComponent(c.Setup, componentSetup, &setup))
	require.NoError(t, unmarshalComponent(c.Transport, componentTransport, &transport))
	mutate(&setup, &transport)
	c.Setup, err = marshalComponent(componentSetup, setup)
	require.NoError(t, err)
	c.Transport, err = marshalComponent(componentTransport, transport)
	require.NoError(t, err)
	return c
}

func TestComponentAssemblyEnforcesSnapshotInvariants(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*connectionSetupComponent, *transportComponent)
	}{
		{"version", func(_ *connectionSetupComponent, tr *transportComponent) { tr.LegacyVersion = 12345 }},
		{"empty-id", func(s *connectionSetupComponent, _ *transportComponent) { s.ID = ""; s.LocalUfrag = "" }},
		{"ufrag", func(s *connectionSetupComponent, _ *transportComponent) { s.LocalUfrag = "not-the-session" }},
		{"address", func(_ *connectionSetupComponent, tr *transportComponent) { tr.RemoteAddr = "invalid AddrPort" }},
		{"profile", func(_ *connectionSetupComponent, tr *transportComponent) { tr.Profile = 0 }},
		{"missing-v2", func(_ *connectionSetupComponent, tr *transportComponent) { tr.Host = nil }},
		{"wrong-contract", func(_ *connectionSetupComponent, tr *transportComponent) {
			var h v2Envelope
			require.NoError(t, json.Unmarshal(tr.Host, &h))
			h.Contract = "another-contract"
			var err error
			tr.Host, err = json.Marshal(h)
			require.NoError(t, err)
		}},
		{"legacy-with-v2", func(_ *connectionSetupComponent, tr *transportComponent) { tr.LegacyVersion = 7 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := mutatedComponents(t, tc.mutate)
			_, _, err := assembleSessionComponents(c, 1)
			require.ErrorIs(t, err, errBadState)
		})
	}
}

func TestComponentCountsAreRejectedBeforeAllocation(t *testing.T) {
	type certs struct{ Certificates [][]byte }
	type counters struct{ Values map[uint32]uint64 }
	// The count fits the remaining byte count but cannot fit the minimum wire
	// bytes per element: 4 for a byte slice and 12 for a uint32/uint64 map pair.
	const count = 128
	data := binary.BigEndian.AppendUint32([]byte{10, componentSetup}, count)
	data = append(data, make([]byte, count)...)
	var chain certs
	require.ErrorIs(t, unmarshalComponent(data, componentSetup, &chain), errBadState)
	require.Nil(t, chain.Certificates, "reject the count before allocating its slice")
	data[1] = componentTransport
	var indexes counters
	require.ErrorIs(t, unmarshalComponent(data, componentTransport, &indexes), errBadState)
	require.Nil(t, indexes.Values, "reject the count before allocating its map")
	oversized := make([]byte, (128<<10)+1)
	oversized[0], oversized[1] = 10, componentHistory
	var raw []byte
	require.ErrorIs(t, unmarshalComponent(oversized, componentHistory, &raw), errBadState)
	require.Nil(t, raw)
}

func TestComponentRefusalsShareBadStateClass(t *testing.T) {
	metadata, err := componentMaxMetadata().marshal()
	require.NoError(t, err)
	for n := 0; n < len(metadata); n++ {
		_, err := decodeComponentMetadata(metadata[:n])
		require.ErrorIs(t, err, errBadState, "metadata prefix %d", n)
	}
	for _, mutate := range []func(*connectionSetupComponent, *transportComponent){
		func(_ *connectionSetupComponent, tr *transportComponent) { tr.Host = []byte("{") },
		func(_ *connectionSetupComponent, tr *transportComponent) { tr.RemoteAddr = "not-an-address" },
	} {
		c := mutatedComponents(t, mutate)
		_, _, err := assembleSessionComponents(c, 1)
		require.ErrorIs(t, err, errBadState)
	}
	master := bytes.Repeat([]byte{0x31}, 32)
	m := componentMaxMetadata()
	sealed, err := sealComponent(master, 255, "session-a", componentWork, m, []byte("notes"))
	require.NoError(t, err)
	sealed[len(sealed)-1] ^= 1
	_, err = openComponent(master, 255, "session-a", componentWork, m, sealed)
	require.ErrorIs(t, err, errBadState)
}

func TestSnapshotValidationKeepsLegacyErrorClasses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*snapshot)
		want   error
	}{
		{"outer-version", func(s *snapshot) { s.Version = 12345 }, errStateVersion},
		{"state-version", func(s *snapshot) { s.State.Version = 7 }, errStateVersion},
		{"ufrag", func(s *snapshot) { s.State.ICE.LocalUfrag = "different" }, errBadState},
		{"profile", func(s *snapshot) { s.State.SRTP.Profile = 0 }, errBadState},
		{"missing-v2", func(s *snapshot) { s.State.Agent.V2 = nil }, errBadState},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := componentFixture(t)
			tc.mutate(&s)
			type plain snapshot
			b, err := json.Marshal(plain(s))
			require.NoError(t, err)
			_, err = decodeSnapshot(b)
			require.True(t, errors.Is(err, tc.want), fmt.Sprint(err))
		})
	}
}

func TestComponentKeysAreSeparatedBySessionKindAndPurpose(t *testing.T) {
	master := bytes.Repeat([]byte{0x63}, 32)
	for _, kind := range []byte{componentSetup, componentTransport, componentWork, componentHistory, componentRoot, componentCheckpoint} {
		for _, purpose := range []string{"encryption", "digest"} {
			a, err := componentKey(master, "session-a", kind, purpose)
			require.NoError(t, err)
			b, err := componentKey(master, "session-b", kind, purpose)
			require.NoError(t, err)
			require.NotEqual(t, a, b, "session-bound %d/%s", kind, purpose)
			repeat, err := componentKey(master, "session-a", kind, purpose)
			require.NoError(t, err)
			require.Equal(t, a, repeat)
		}
	}
	encryption, err := componentKey(master, "a/\x00/b", componentWork, "encryption")
	require.NoError(t, err)
	digest, err := componentKey(master, "a/\x00/b", componentWork, "digest")
	require.NoError(t, err)
	require.NotEqual(t, encryption, digest)
	history, err := componentKey(master, "a/\x00/b", componentHistory, "encryption")
	require.NoError(t, err)
	require.NotEqual(t, encryption, history)
	_, err = componentKey(master, "", componentWork, "encryption")
	require.ErrorIs(t, err, errBadState)
}

func TestComponentBytePayloadsKeepConfiguredMaximum(t *testing.T) {
	// The documented configurable work ceiling is 64 KiB; the wire cap must
	// include its six framing bytes. The byte fast path still returns owned data.
	raw := bytes.Repeat([]byte{0xff}, 64<<10)
	encoded, err := marshalComponent(componentWork, raw)
	require.NoError(t, err)
	var restored []byte
	require.NoError(t, unmarshalComponent(encoded, componentWork, &restored))
	require.Equal(t, raw, restored)
	restored[0] = 0
	require.Equal(t, byte(0xff), encoded[6])
	require.Equal(t, byte(0xff), raw[0])
}

func TestComponentCaptureNormalizesMappedNomination(t *testing.T) {
	snap := componentFixture(t)
	snap.State.ICE.RemoteAddr = netip.MustParseAddrPort("[::ffff:127.0.0.1]:53458")
	c, err := splitSessionComponents(snap, nil, nil)
	require.NoError(t, err)
	assembled, _, err := assembleSessionComponents(c, snap.State.Agent.State.Version)
	require.NoError(t, err)
	require.Equal(t, netip.MustParseAddrPort("127.0.0.1:53458"), assembled.State.ICE.RemoteAddr)
}
