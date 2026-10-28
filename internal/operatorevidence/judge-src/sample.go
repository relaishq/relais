// Shared raw generator evidence decoder; copied into the pinned judge package.
package main

import (
	"fmt"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorgates/observe"
)

func readSample(raw []byte, pid int) (loadgen.Stats, error) {
	var m loadgen.Message
	if err := observe.DecodeEvidenceLimit(raw, &m, 64<<20); err != nil {
		return loadgen.Stats{}, err
	}
	if err := observe.Numbers(m); err != nil {
		return loadgen.Stats{}, err
	}
	s := m.Stats
	if m.Type != "stats" || s == nil || pid != 0 && m.PID != pid || s.At.IsZero() || s.Interval <= 0 || s.CPUPerGOMAXPROCS < 0 || s.IntervalLateness.P99 < 0 || s.IntervalLateness.Max < 0 {
		return loadgen.Stats{}, fmt.Errorf("missing or invalid generator stats fields")
	}
	if _, ok := observe.AddCount(int64(s.IntervalPacing.SentFrames), int64(s.IntervalPacing.SkippedFrames)); !ok {
		return loadgen.Stats{}, fmt.Errorf("pacing total overflow")
	}
	if _, ok := observe.AddCount(int64(s.Pacing.SentFrames), int64(s.Pacing.SkippedFrames)); !ok {
		return loadgen.Stats{}, fmt.Errorf("pacing total overflow")
	}
	return *s, nil
}
