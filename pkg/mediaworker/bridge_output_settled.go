package mediaworker

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"unsafe"

	agent "github.com/relaishq/relais/pkg/agent/v2"
)

// Sent identities cannot reenter playback. Keep their conflict evidence in a
// packed, immutable prefix rather than duplicating scope/receipt/queue headers.
// Entries are SHA-256 followed by a positive receipt-revision delta uvarint.
// The chain binds each digest to its scope, sequence, turn, stream and revision. Keep
// individual digests too: a chain alone cannot answer an arbitrary retry.
// Append writes only beyond the frozen length; exports may share old prefixes.
type bridgeSettledRange struct {
	Scope        bridgeOutputIdentity `json:"scope"`
	BaseRevision uint64               `json:"base_revision"`
	lastRevision uint64
	First        uint64       `json:"first"`
	Last         uint64       `json:"last"`
	Turn         agent.Turn   `json:"turn"`
	Stream       uint64       `json:"stream"`
	Seed         bridgeDigest `json:"seed"`
	Chain        bridgeDigest `json:"chain"`
	Data         []byte       `json:"data"`
}

const bridgeRecentSent = 1
const bridgeSettledEntries = 256
const bridgeSettledHeaderCharge = 512
const bridgeSettledFrozenHeaderCharge = int64((unsafe.Sizeof(bridgeSettledRange{}) + 63) &^ 63)

func bridgeSettledChain(scope bridgeOutputIdentity, prior [32]byte, seq uint64, turn agent.Turn, stream, revision uint64, digest [32]byte) [32]byte {
	var identity [16]byte
	binary.BigEndian.PutUint64(identity[:8], scope.Epoch)
	binary.BigEndian.PutUint64(identity[8:], scope.Generation)
	scopeBytes := []byte(scope.SessionID + "\x00" + scope.BotInstance + "\x00" + scope.LineageID + "\x00")
	scopeBytes = append(scopeBytes, identity[:]...)
	scopeHash := sha256.Sum256(scopeBytes)
	var entry [128]byte
	copy(entry[96:], scopeHash[:])
	copy(entry[:32], prior[:])
	binary.BigEndian.PutUint64(entry[32:40], seq)
	binary.BigEndian.PutUint64(entry[40:48], uint64(turn))
	binary.BigEndian.PutUint64(entry[48:56], stream)
	binary.BigEndian.PutUint64(entry[56:64], revision)
	copy(entry[64:], digest[:])
	return sha256.Sum256(entry[:])
}
func bridgeSettledEntry(data []byte) (digest [32]byte, revision uint64, n int) {
	if len(data) < 33 {
		return digest, 0, 0
	}
	copy(digest[:], data[:32])
	revision, n = binary.Uvarint(data[32:])
	if n <= 0 || revision == 0 {
		return digest, 0, 0
	}
	return digest, revision, n + 32
}
func bridgeSettledMemory(r bridgeSettledRange) int64 {
	return bridgeSettledHeaderCharge + int64(cap(r.Data))
}
func (b *bridgeOutputHost) settledMemory() int64 { return b.settledLive.Load() }

// These hashes use base64 JSON, avoiding numeric-array overhead in every block.
type bridgeDigest [sha256.Size]byte

func (d bridgeDigest) MarshalJSON() ([]byte, error) {
	out := make([]byte, 1, 46)
	out[0] = '"'
	out = base64.StdEncoding.AppendEncode(out, d[:])
	return append(out, '"'), nil
}
func (d *bridgeDigest) UnmarshalJSON(data []byte) error {
	var value []byte
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if len(value) != sha256.Size {
		return fmt.Errorf("invalid bridge digest length")
	}
	copy(d[:], value)
	return nil
}
func bridgeSettledJSONBytes(r bridgeSettledRange) int64 {
	scope := r.Scope
	n := len(`{"s":"","b":"","e":,"g":,"l":""}`) + len(scope.SessionID) + len(scope.BotInstance) + len(scope.LineageID) + len(strconv.FormatUint(scope.Epoch, 10)) + len(strconv.FormatUint(scope.Generation, 10))
	n += len(`{"scope":,"first":,"last":,"turn":,"stream":,"base_revision":,"seed":,"chain":,"data":}`) + 3*2 + 2*base64.StdEncoding.EncodedLen(32)
	for _, v := range []uint64{r.First, r.Last, uint64(r.Turn), r.Stream, r.BaseRevision} {
		n += len(strconv.FormatUint(v, 10))
	}
	return int64(n) + bridgeBase64Bytes(int64(len(r.Data)))
}
func bridgeSettledCost(r bridgeSettledRange) int64 {
	return bridgeSettledMemory(r) + bridgeSettledFrozenHeaderCharge + bridgeJSONBuffers(bridgeSettledJSONBytes(r)+2)
}
func (b *bridgeOutputHost) replaceSettledCost(old, next *bridgeSettledRange) {
	if old != nil {
		b.settledLive.Add(-bridgeSettledMemory(*old))
		b.settledCost.Add(-bridgeSettledCost(*old))
	}
	if next != nil {
		b.settledLive.Add(bridgeSettledMemory(*next))
		b.settledCost.Add(bridgeSettledCost(*next))
	}
}

// Restore initializes the totals once. Hot append/prune update only changed blocks.
func (b *bridgeOutputHost) updateSettledCost() {
	b.settledLive.Store(0)
	b.settledCost.Store(0)
	for _, r := range b.settled {
		b.replaceSettledCost(nil, &r)
	}
}
func bridgeSettledLastRevision(r bridgeSettledRange) uint64 {
	revision := r.BaseRevision
	for data := r.Data; len(data) > 0; {
		_, delta, n := bridgeSettledEntry(data)
		if n == 0 {
			return 0
		}
		revision += delta
		data = data[n:]
	}
	return revision
}
func bridgeSettledContains(ranges []bridgeSettledRange, seq uint64) bool {
	return slices.ContainsFunc(ranges, func(r bridgeSettledRange) bool { return seq >= r.First && seq <= r.Last })
}
func (b *bridgeOutputHost) settledReceipt(seq uint64) (BridgeOutputReceipt, [32]byte, bool) {
	for _, r := range b.settled {
		if r.Scope != b.state.Scope || seq < r.First || seq > r.Last {
			continue
		}
		data := r.Data
		revision := r.BaseRevision
		for at := r.First; at <= seq; at++ {
			digest, delta, n := bridgeSettledEntry(data)
			if n == 0 {
				break
			}
			revision += delta
			if at == seq {
				return BridgeOutputReceipt{BridgeOutputScope: BridgeOutputScope(r.Scope), Sequence: seq, Turn: r.Turn, Revision: revision, Outcome: "sent", Consumed: true}, digest, true
			}
			data = data[n:]
		}
	}
	return BridgeOutputReceipt{}, [32]byte{}, false
}

func (s *session) compactBridgeSentLocked(sent uint64) {
	if sent <= bridgeRecentSent {
		return
	}
	b := s.agent.v2.bridge
	seq := sent - bridgeRecentSent
	record, ok := b.records[seq]
	if !ok || record.Receipt.Outcome != "sent" || record.Payload != nil || record.Scope != BridgeOutputScope(b.state.Scope) {
		return
	}
	r := bridgeSettledRange{Scope: bridgeOutputIdentity(record.Scope), First: seq, Last: seq - 1, Turn: record.Receipt.Turn, Stream: record.Stream}
	merge := len(b.settled) > 0 && b.settled[len(b.settled)-1].Scope == r.Scope && b.settled[len(b.settled)-1].Last+1 == seq && b.settled[len(b.settled)-1].Turn == r.Turn && b.settled[len(b.settled)-1].Stream == r.Stream && b.settled[len(b.settled)-1].Last-b.settled[len(b.settled)-1].First+1 < bridgeSettledEntries
	var old, oldCost int64
	var prior *bridgeSettledRange
	if merge {
		r = b.settled[len(b.settled)-1]
		old = bridgeSettledMemory(r)
		oldCost = bridgeSettledCost(r)
		clone := r
		prior = &clone
	}
	var entry [42]byte
	copy(entry[:32], record.Digest[:])
	n := 32 + binary.PutUvarint(entry[32:], record.Receipt.Revision-r.lastRevision)
	if len(r.Data)+n > cap(r.Data) {
		data := make([]byte, len(r.Data), len(r.Data)+n+1024)
		copy(data, r.Data)
		r.Data = data
	}
	r.Data = append(r.Data, entry[:n]...)
	r.Last = seq
	r.Chain = bridgeSettledChain(r.Scope, r.Chain, seq, r.Turn, r.Stream, record.Receipt.Revision, record.Digest)
	r.lastRevision = record.Receipt.Revision
	if s.bridgeExportCostLocked()+bridgeSettledCost(r)-oldCost-bridgePacketExportCost(record) > s.bridgeLookaheadBudget() {
		return
	}
	if err := s.reserveBridgeMemory(bridgeSettledMemory(r) - old - bridgeRecordCharge); err != nil {
		return
	}
	if merge {
		b.settled[len(b.settled)-1] = r
	} else {
		b.settled = append(b.settled, r)
	}
	b.replaceRecordEncoding(&record, nil)
	delete(b.records, seq)
	b.replaceSettledCost(prior, &r)
}
func (s *session) pruneBridgeSettledLocked(through uint64) {
	b := s.agent.v2.bridge
	old := b.settledMemory()
	kept := make([]bridgeSettledRange, 0, len(b.settled))
	for _, r := range b.settled {
		oldRange := r
		if r.Last <= through {
			b.replaceSettledCost(&oldRange, nil)
			continue
		}
		if r.First <= through {
			for r.First <= through {
				digest, delta, n := bridgeSettledEntry(r.Data)
				r.BaseRevision += delta
				r.Seed = bridgeSettledChain(r.Scope, r.Seed, r.First, r.Turn, r.Stream, r.BaseRevision, digest)
				r.Data = r.Data[n:]
				r.First++
			}
			r.Data = slices.Clone(r.Data)
			b.replaceSettledCost(&oldRange, &r)
		}
		kept = append(kept, r)
	}
	b.settled = kept
	_ = s.reserveBridgeMemory(b.settledMemory() - old)
}

func validateBridgeSettled(image *bridgeOutputMove, saved *bridgeOutputState) (uint64, error) {
	if image.Version == bridgeOutputVersion && len(image.Settled) > 0 {
		return 0, agent.ErrIncompatibleAgent
	}
	var count, previous uint64
	for _, r := range image.Settled {
		if r.Scope != saved.Scope || r.BaseRevision > saved.Revision || r.First <= saved.Pruned || r.First <= previous || r.Last < r.First || r.Last == ^uint64(0) || r.Last > saved.Sent || r.Stream == 0 || r.Stream > saved.LastStream || r.Turn > saved.LastTurn || r.Last-r.First+1 > bridgeSettledEntries || r.Last-r.First+1 > uint64(len(r.Data)/33) {
			return 0, agent.ErrIncompatibleAgent
		}
		chain, data := r.Seed, r.Data
		revision := r.BaseRevision
		for seq := r.First; seq <= r.Last; seq++ {
			digest, delta, n := bridgeSettledEntry(data)
			if n == 0 || delta > saved.Revision-revision {
				return 0, agent.ErrIncompatibleAgent
			}
			revision += delta
			chain = bridgeSettledChain(r.Scope, chain, seq, r.Turn, r.Stream, revision, digest)
			data = data[n:]
			// A packed sent identity must not overlap a checkpoint exception.
			if slices.ContainsFunc(saved.Exceptions, func(e bridgeReceiptRange) bool { return seq >= e.First && seq <= e.Last }) {
				return 0, agent.ErrIncompatibleAgent
			}
		}
		if len(data) > 0 || chain != r.Chain {
			return 0, agent.ErrIncompatibleAgent
		}
		previous = r.Last
		count += r.Last - r.First + 1
	}
	return count, nil
}
