package controlplane

import "strings"

// The answer, not the caller's offer, determines the transport policy: an
// opted-out worker still rejects application media in its answer. This cached
// classification avoids a new private HTTP round trip for ordinary media moves.
// Crash recovery independently reads the durable version-nine marker.
func answerHasDataChannel(answer string) bool {
	for _, line := range strings.Split(answer, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 && fields[0] == "m=application" && fields[1] != "0" && fields[2] == "UDP/DTLS/SCTP" && fields[3] == "webrtc-datachannel" {
			return true
		}
	}
	return false
}
