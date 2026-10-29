package operatorevidence

import (
	"strings"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorgates/observe"
)

// JoinedDuplicatePackets requires the exact caller process, session population
// and both loaded tracks. Missing evidence remains absent, never a clean zero.
func JoinedDuplicatePackets(raw string, sessions []string, pid int) (*uint64, error) {
	if pid <= 0 || uint64(pid) > 9007199254740991 {
		return nil, nil
	}
	expected := map[string]bool{}
	for _, s := range sessions {
		if expected[s] {
			return nil, nil
		}
		expected[s] = true
	}
	if len(expected) == 0 {
		return nil, nil
	}
	var total uint64
	calls := 0
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m loadgen.Message
		if err := observe.DecodeLimit([]byte(line), &m, observe.Contract{"type"}, 64<<20); err != nil {
			return nil, err
		}
		if m.Type != "call" {
			continue
		}
		calls++
		if m.PID != pid || !expected[m.Session] {
			return nil, nil
		}
		delete(expected, m.Session)
		if m.Call == nil || observe.Require([]byte(line), observe.Contract{"pid", "session", "call.Tracks[].Kind", "call.Tracks[].Packets", "call.Tracks[].DuplicatePackets"}) != nil {
			return nil, nil
		}
		audio, video := false, false
		for _, track := range m.Call.Tracks {
			if track.Packets > 0 {
				audio = audio || track.Kind == "audio"
				video = video || track.Kind == "video"
			}
			if track.DuplicatePackets < 0 || uint64(track.DuplicatePackets) > 9007199254740991-total {
				return nil, nil
			}
			total += uint64(track.DuplicatePackets)
		}
		if !audio || !video {
			return nil, nil
		}
	}
	if calls != len(sessions) || len(expected) != 0 {
		return nil, nil
	}
	return &total, nil
}
