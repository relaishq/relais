package mediaworker

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"

	agent "github.com/relaishq/relais/pkg/agent/v2"
)

// Exact JSON length arithmetic avoids encoding retained audio under the media
// lock. Scope tokens and receipt enums cannot contain JSON escape characters.
func bridgeScopeJSONBytes(v BridgeOutputScope) int {
	return len(`{"session_id":"","bot_instance":"","epoch":,"generation":,"lineage_id":""}`) + len(v.SessionID) + len(v.BotInstance) + len(v.LineageID) + len(strconv.FormatUint(v.Epoch, 10)) + len(strconv.FormatUint(v.Generation, 10))
}
func bridgeRecordJSONBytes(r bridgePacketRecord) int {
	v := r.Receipt
	receipt := bridgeScopeJSONBytes(v.BridgeOutputScope) - 1 + len(`,"seq":,"turn":,"revision":,"consumed":`) + len(strconv.FormatUint(v.Sequence, 10)) + len(strconv.FormatUint(uint64(v.Turn), 10)) + len(strconv.FormatUint(v.Revision, 10)) + len(strconv.FormatBool(v.Consumed)) + 1
	for _, field := range []struct{ name, value string }{{"outcome", v.Outcome}, {"reason", v.Reason}, {"status", v.Status}} {
		if field.value != "" {
			receipt += len(field.name) + len(field.value) + 6
		}
	}
	digest := 2 + len(r.Digest) - 1
	for _, b := range r.Digest {
		digest += len(strconv.Itoa(int(b)))
	}
	n := len(`{"scope":,"receipt":,"digest":,"duration":,"length":,"stream":}`) + bridgeScopeJSONBytes(r.Scope) + receipt + digest + len(strconv.FormatInt(int64(r.Duration), 10)) + len(strconv.Itoa(r.Length)) + len(strconv.FormatUint(r.Stream, 10))
	if len(r.Payload) > 0 {
		n += len(`,"payload":""`) + base64.StdEncoding.EncodedLen(len(r.Payload))
	}
	return n
}
func bridgeRecordQueueJSONBytes(r bridgePacketRecord) int {
	// Every record reserves a full reference, even after it leaves the live queue.
	return len(`{"packet_seq":,"last":,"turn":,"stream_id":,"duration":,"kind":"bridge","offset":0,"count":1}`) + 2*len(strconv.FormatUint(r.Receipt.Sequence, 10)) + len(strconv.FormatUint(uint64(r.Receipt.Turn), 10)) + len(strconv.FormatUint(r.Stream, 10)) + len(strconv.FormatInt(int64(r.Duration), 10))
}
func (b *bridgeOutputHost) replaceRecordEncoding(old, next *bridgePacketRecord) {
	if old != nil {
		b.recordMemory.Add(-int64(bridgeRecordCharge + len(old.Payload)))
		b.encodingBytes.Add(-int64(bridgeRecordJSONBytes(*old) + bridgeRecordQueueJSONBytes(*old) + 2))
		b.recordCount.Add(-1)
	}
	if next != nil {
		b.recordMemory.Add(int64(bridgeRecordCharge + len(next.Payload)))
		b.encodingBytes.Add(int64(bridgeRecordJSONBytes(*next) + bridgeRecordQueueJSONBytes(*next) + 2))
		b.recordCount.Add(1)
	}
}

func validateExactWire(data []byte, bridge bool) error {
	if !bridge {
		return validateV2Wire(data)
	}
	for _, v := range []any{ExportReply{State: []byte{0, 0, 0}}, ResumeRequest{State: []byte{0, 0, 0}}} {
		encoded, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if len(encoded)-4+base64.StdEncoding.EncodedLen(len(data)) >= v2MaxWireBytes-(64<<10) {
			return fmt.Errorf("%w: exact export exceeds private API wire budget", agent.ErrCapacity)
		}
	}
	return nil
}
