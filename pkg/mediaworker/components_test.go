package mediaworker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/agent"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func componentFixture(t *testing.T) snapshot {
	t.Helper()
	data, err := os.ReadFile("testdata/v9-control.json")
	require.NoError(t, err)
	snap, err := decodeSnapshot(data)
	require.NoError(t, err)
	return *snap
}
func componentGolden(t *testing.T, name string, data []byte) {
	t.Helper()
	path := "testdata/v10-" + name + ".hex"
	if os.Getenv("RELAIS_UPDATE_COMPONENT_GOLDEN") == "1" {
		require.NoError(t, os.WriteFile(path, []byte(hex.EncodeToString(data)+"\n"), 0600))
	}
	golden, err := os.ReadFile(path)
	require.NoError(t, err)
	decoded, err := hex.DecodeString(strings.TrimSpace(string(golden)))
	require.NoError(t, err)
	require.Equal(t, decoded, data, "component golden %s", name)
}

func TestComponentEncodingsAreCanonical(t *testing.T) {
	for _, tc := range []struct {
		name    string
		history []byte
	}{{"empty", nil}, {"full", []byte("four bounded exchanges: \"hello\" / \u4f60\u597d")}} {
		t.Run(tc.name, func(t *testing.T) {
			snap := componentFixture(t)
			c, err := splitSessionComponents(snap, tc.history, nil)
			require.NoError(t, err)
			for _, part := range []struct {
				name string
				data []byte
			}{{"setup", c.Setup}, {"transport", c.Transport}, {"work", c.Work}, {tc.name + "-history", c.History}} {
				componentGolden(t, part.name, part.data)
			}
			restored, history, err := assembleSessionComponents(c, snap.State.Agent.State.Version)
			require.NoError(t, err)
			require.True(t, bytes.Equal(tc.history, history))
			require.Equal(t, snap.State.DTLS, restored.State.DTLS)
			require.Equal(t, snap.State.ICE, restored.State.ICE)
			require.Equal(t, snap.State.Audio, restored.State.Audio)
			require.Equal(t, snap.State.Video, restored.State.Video)
			require.Equal(t, snap.State.Agent.V2, restored.State.Agent.V2)
			again, err := splitSessionComponents(restored, history, c.Setup)
			require.NoError(t, err)
			require.Equal(t, c, again, "re-encode must equal canonical input")
		})
	}
}

func TestComponentDTLSSetupExcludesChangingFields(t *testing.T) {
	snap := componentFixture(t)
	before, err := splitSessionComponents(snap, nil, nil)
	require.NoError(t, err)
	original, err := decodeDTLSSerializedState(snap.DTLSConnection)
	require.NoError(t, err)
	for _, name := range []string{"LocalEpoch", "RemoteEpoch", "SequenceNumber"} {
		t.Run(name, func(t *testing.T) {
			state := original
			field := reflect.ValueOf(&state).Elem().FieldByName(name)
			field.SetUint(field.Uint() + 1)
			var b bytes.Buffer
			require.NoError(t, gob.NewEncoder(&b).Encode(state))
			snap.DTLSConnection = b.Bytes()
			after, err := splitSessionComponents(snap, nil, before.Setup)
			require.NoError(t, err)
			require.Equal(t, before.Setup, after.Setup)
			require.NotEqual(t, before.Transport, after.Transport)
			joined, _, err := assembleSessionComponents(after, snap.State.Agent.State.Version)
			require.NoError(t, err)
			decoded, err := decodeDTLSSerializedState(joined.DTLSConnection)
			require.NoError(t, err)
			require.Equal(t, state, decoded, "every pinned Pion field must survive")
		})
	}
	snap = componentFixture(t)
	snap.State.DTLS.PrivateKey = append(snap.State.DTLS.PrivateKey, 1)
	_, err = splitSessionComponents(snap, nil, before.Setup)
	require.ErrorContains(t, err, "changed connection setup")
}

func TestComponentCrashRestoreNeverRepeatsDTLSSequence(t *testing.T) {
	for _, kind := range []agent.ResumeKind{agent.Takeover, agent.PlannedMove} {
		t.Run(string(kind), func(t *testing.T) {
			store := sessionstore.NewMemory()
			snap := dtlsFixture(t)
			lease, err := store.Claim(context.Background(), snap.State.ID, netip.MustParseAddrPort("127.0.0.1:10000"), time.Hour)
			require.NoError(t, err)
			old, wire := dtlsCheckpointSession(t, store, snap, lease, ResumeOptions{})
			require.NoError(t, old.persistSnapshot())
			captured, err := store.GetState(context.Background(), old.id)
			require.NoError(t, err)
			saved, err := decodeSnapshot(captured)
			require.NoError(t, err)
			components, err := splitSessionComponents(*saved, []byte("durable conversation"), nil)
			require.NoError(t, err)
			seen := make(map[[8]byte]bool)
			if kind == agent.Takeover {
				for range 64 {
					_, err = old.dtlsConn.Write([]byte("stale-tail"))
					require.NoError(t, err)
					seen[dtlsNonceInput(t, dtlsPacket(t, wire))] = true
				}
				_, err = old.dtlsConn.Write([]byte("past-bound"))
				require.NoError(t, err)
				require.Empty(t, wire.packets, "v10 source must still obey the production guard")
				require.EqualValues(t, 1, old.worker.metrics.dtlsSequenceDrops.Load())
			}
			old.fenced.Store(true)
			old.close() // exact export fences before adoption
			lease, err = store.Transfer(context.Background(), lease, netip.MustParseAddrPort("127.0.0.1:10001"), time.Hour)
			require.NoError(t, err)
			assembled, history, err := assembleSessionComponents(components, saved.State.Agent.State.Version)
			require.NoError(t, err)
			require.Equal(t, "durable conversation", string(history))
			restored, restoredWire := dtlsCheckpointSession(t, store, assembled, lease, ResumeOptions{Kind: kind})
			require.NoError(t, restored.persistSnapshot())
			_, err = restored.dtlsConn.Write([]byte("replacement"))
			require.NoError(t, err)
			pair := dtlsNonceInput(t, dtlsPacket(t, restoredWire))
			require.False(t, seen[pair], "v10 restore reused epoch/sequence %x", pair)
			expected := uint64(1)
			if kind == agent.Takeover {
				expected = 65
			}
			require.Equal(t, uint64(1)<<48|expected, binary.BigEndian.Uint64(pair[:]))
		})
	}
}

func TestComponentDecoderRefusesNoncanonicalBytes(t *testing.T) {
	type sample struct {
		Values map[uint32]uint64
		Flag   bool
	}
	value := sample{map[uint32]uint64{2: 20, 1: 10}, true}
	data, err := marshalComponent(componentTransport, value)
	require.NoError(t, err)
	unordered := bytes.Clone(data)
	copy(unordered[6:18], data[18:30])
	copy(unordered[18:30], data[6:18])
	invalidBool := bytes.Clone(data)
	invalidBool[len(data)-1] = 2
	hugeCount := bytes.Clone(data)
	binary.BigEndian.PutUint32(hugeCount[2:6], math.MaxUint32)
	for _, bad := range [][]byte{append(bytes.Clone(data), 0), data[:len(data)-1], append([]byte{11}, data[1:]...), unordered, invalidBool, hugeCount} {
		var decoded sample
		require.Error(t, unmarshalComponent(bad, componentTransport, &decoded))
	}
	var decoded sample
	require.NoError(t, unmarshalComponent(data, componentTransport, &decoded))
	require.Equal(t, value, decoded)
	var wrong sample
	require.Error(t, unmarshalComponent(data, componentSetup, &wrong))
}

func componentMaxMetadata() componentMetadata {
	m := componentMetadata{Format: 10, Owner: strings.Repeat("W", 64), OwnerEpoch: math.MaxUint64, StateVersion: math.MaxUint32, Banks: [3]byte{1, 1, 1}, SetupGeneration: math.MaxUint64}
	for i := range m.References {
		m.References[i] = componentReference{KeyID: 255, Revision: math.MaxUint64}
		for j := range m.References[i].Digest {
			m.References[i].Digest[j] = byte(i*32 + j)
		}
	}
	return m
}

func TestComponentMetadataReserveAndGolden(t *testing.T) {
	m := componentMaxMetadata()
	packed, err := m.marshal()
	require.NoError(t, err)
	require.Len(t, packed, 295)
	require.Equal(t, 384, len(packed)+componentCommitTimeBytes+componentFieldNameBytes+componentProtocolReserve)
	require.Equal(t, 384, componentReferenceReserve)
	componentGolden(t, "metadata", packed)
	componentGolden(t, "encryption-header", []byte{1, 255})
	decoded, err := decodeComponentMetadata(packed)
	require.NoError(t, err)
	require.Equal(t, m, decoded)
	_, err = decodeComponentMetadata(append(packed, 0))
	require.Error(t, err)
	m.Owner += "x"
	_, err = m.marshal()
	require.Error(t, err)
}

func TestComponentAuthenticatedReferences(t *testing.T) {
	master := bytes.Repeat([]byte{0x45}, 32)
	snap := componentFixture(t)
	c, err := splitSessionComponents(snap, []byte("history"), nil)
	require.NoError(t, err)
	m := componentMaxMetadata()
	m.StateVersion = snap.State.Agent.State.Version
	sealed, m, err := sealSessionComponents(master, 255, snap.State.ID, m, c)
	require.NoError(t, err)
	decoded, err := openSessionComponents(map[uint8][]byte{255: master}, snap.State.ID, m, sealed)
	require.NoError(t, err)
	require.Equal(t, c, decoded)
	for _, parts := range [][2][]byte{{c.Setup, sealed.Setup}, {c.Transport, sealed.Transport}, {c.Work, sealed.Work}, {c.History, sealed.History}} {
		require.Equal(t, len(parts[0])+30, len(parts[1]))
	}
	// Random nonces are fresh; identical content has stable keyed digests.
	other, m2, err := sealSessionComponents(master, 255, snap.State.ID, m, c)
	require.NoError(t, err)
	require.NotEqual(t, sealed, other)
	require.Equal(t, m, m2)
	mutations := []func(*componentMetadata){func(m *componentMetadata) { m.Owner = "other" }, func(m *componentMetadata) { m.OwnerEpoch-- }, func(m *componentMetadata) { m.StateVersion++ }, func(m *componentMetadata) { m.SetupGeneration-- }}
	for i := range m.References {
		mutations = append(mutations, func(m *componentMetadata) { m.References[i].Digest[0] ^= 1 }, func(m *componentMetadata) { m.References[i].Revision-- }, func(m *componentMetadata) { m.References[i].KeyID-- })
	}
	for i := range m.Banks {
		mutations = append(mutations, func(m *componentMetadata) { m.Banks[i] ^= 1 })
	}
	for i, mutate := range mutations {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			bad := m
			mutate(&bad)
			_, err := openSessionComponents(map[uint8][]byte{255: master}, snap.State.ID, bad, sealed)
			require.Error(t, err)
		})
	}
	_, err = openSessionComponents(map[uint8][]byte{255: master}, "another-session", m, sealed)
	require.Error(t, err)
	tampered := sealed
	tampered.Work = bytes.Clone(sealed.Work)
	tampered.Work[2] ^= 1
	_, err = openSessionComponents(map[uint8][]byte{255: master}, snap.State.ID, m, tampered)
	require.Error(t, err)
	// Unchanged setup/app ciphertexts survive owner transfers; only checkpoint
	// ciphertext authenticates its writer/epoch and the full published tuple.
	moved := m
	moved.Owner = "new-owner"
	moved.OwnerEpoch--
	for _, part := range []struct {
		kind          byte
		sealed, plain []byte
	}{{componentSetup, sealed.Setup, c.Setup}, {componentWork, sealed.Work, c.Work}, {componentHistory, sealed.History, c.History}} {
		plain, err := openComponent(master, 255, snap.State.ID, part.kind, moved, part.sealed)
		require.NoError(t, err)
		require.Equal(t, part.plain, plain)
	}
	work, err := componentDigest(master, componentWork, []byte("same"))
	require.NoError(t, err)
	history, err := componentDigest(master, componentHistory, []byte("same"))
	require.NoError(t, err)
	require.NotEqual(t, work, history)
	key2 := bytes.Repeat([]byte{0x46}, 32)
	different, err := componentDigest(key2, componentWork, []byte("same"))
	require.NoError(t, err)
	require.NotEqual(t, work, different)
}

func TestComponentHostMetadataIsCanonical(t *testing.T) {
	snap := componentFixture(t)
	c, err := splitSessionComponents(snap, nil, nil)
	require.NoError(t, err)
	var transport transportComponent
	require.NoError(t, unmarshalComponent(c.Transport, componentTransport, &transport))
	transport.Host = append(transport.Host[:len(transport.Host)-1], []byte(",\"unknown\":true}")...)
	c.Transport, err = marshalComponent(componentTransport, transport)
	require.NoError(t, err)
	_, _, err = assembleSessionComponents(c, snap.State.Agent.State.Version)
	require.ErrorContains(t, err, "noncanonical host metadata")
	// Ordinary v9 JSON remains exactly as it was, outside this codec.
	_, err = json.Marshal(snap)
	require.NoError(t, err)
}

func TestComponentDTLSAdapterPreservesPinnedFields(t *testing.T) {
	source, err := decodeDTLSSerializedState(dtlsFixture(t).DTLSConnection)
	require.NoError(t, err)
	source.LocalEpoch, source.RemoteEpoch, source.SequenceNumber = 2, 3, 255
	source.IdentityHint = []byte("identity")
	source.SessionID = []byte("session")
	source.LocalConnectionID = []byte("local")
	source.RemoteConnectionID = []byte("remote")
	source.NegotiatedProtocol = "test-protocol"
	source.IsClient = true
	var encoded bytes.Buffer
	require.NoError(t, gob.NewEncoder(&encoded).Encode(source))
	setup, progress, err := splitDTLSComponents(encoded.Bytes())
	require.NoError(t, err)
	setupBytes, err := marshalComponent(componentSetup, setup)
	require.NoError(t, err)
	progressBytes, err := marshalComponent(componentTransport, progress)
	require.NoError(t, err)
	var decodedSetup componentDTLSSetup
	var decodedProgress componentDTLSProgress
	require.NoError(t, unmarshalComponent(setupBytes, componentSetup, &decodedSetup))
	require.NoError(t, unmarshalComponent(progressBytes, componentTransport, &decodedProgress))
	joined, err := joinDTLSComponents(decodedSetup, decodedProgress)
	require.NoError(t, err)
	actual, err := decodeDTLSSerializedState(joined)
	require.NoError(t, err)
	require.Equal(t, source, actual)
}

func TestComponentCodecLeavesLegacyGoldenBytesUnchanged(t *testing.T) {
	if os.Getenv("RELAIS_UPDATE_COMPONENT_GOLDEN") == "1" {
		snap := componentFixture(t)
		snap.Version, snap.State.Version = v2SessionStateVersion, v2SessionStateVersion
		snap.State.Agent.V2.Controls = nil
		snap.State.Agent.V2.ControlFloor = 0
		snap.State.Agent.V2.ControlHigh = 0
		data, err := json.Marshal(snap)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile("testdata/v8-components-source.json", data, 0600))
	}
	for _, name := range []string{"v7-787127e.json", "v8-components-source.json", "v9-control.json"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + name)
			require.NoError(t, err)
			snap, err := decodeSnapshot(data)
			require.NoError(t, err)
			encoded, err := json.Marshal(snap)
			require.NoError(t, err)
			require.Equal(t, data, encoded, "legacy wire representation must remain byte exact")
		})
	}
}
