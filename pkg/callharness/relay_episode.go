package callharness

import (
	"slices"
	"time"

	"github.com/relaishq/relais/internal/relayloss"
)

// RelayRestartEpisode contains caller-clock observations, never a pass flag.
// The R judge must bind the generation/candidate and require zero arrivals on
// every track in (LastBeforeNS, AppliedNS]. Real network ordering is exercised
// by the later part R rehearsal; these observations do not assume a TTL fence.
type RelayRestartEpisode struct {
	RestartStartNS        time.Duration       `json:"restartStartNs"`
	AppliedNS             time.Duration       `json:"appliedNs"`
	Generation            uint64              `json:"generation"`
	AnswerCandidate       string              `json:"answerCandidate"`
	LastBeforeNS          time.Duration       `json:"lastBeforeNs"`
	FirstVideoNS          time.Duration       `json:"firstVideoNs"`
	FirstAudioNS          time.Duration       `json:"firstAudioNs"`
	ArrivalsBeforeApplied []RelayArrivalCount `json:"arrivalsBeforeApplied"`
}

type RelayArrivalCount struct {
	Kind  string `json:"kind"`
	SSRC  uint32 `json:"ssrc"`
	Count int    `json:"count"`
}

type relayEpisodeRecord struct {
	RelayRestartEpisode
	final bool
}

func (r *recorder) beginRelayRestart(at time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.relayLossEvidence || r.hungUp {
		return -1
	}
	// Retrying an outstanding offer belongs to the first attempt's episode.
	if n := len(r.relayEpisodes); n > 0 && !r.relayEpisodes[n-1].final && r.relayEpisodes[n-1].AppliedNS == 0 {
		return n - 1
	}
	start := r.since(at)
	last, _, _ := r.relayRecoveryPoints(start, start, start)
	r.relayEpisodes = append(r.relayEpisodes, relayEpisodeRecord{RelayRestartEpisode: RelayRestartEpisode{RestartStartNS: start, LastBeforeNS: last}})
	return len(r.relayEpisodes) - 1
}

func (r *recorder) appliedRelayRestart(index int, at time.Time, generation uint64, candidate string) {
	if index < 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := &r.relayEpisodes[index]
	if e.final {
		return
	}
	e.AppliedNS, e.Generation, e.AnswerCandidate = r.since(at), generation, candidate
}

func (r *recorder) observeRelayRestarts(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observeRelayRestartsLocked(r.since(at))
}

func (r *recorder) observeRelayRestartsLocked(now time.Duration) {
	if r.hungUp {
		now = min(now, r.hungUpAt)
	}
	for i := range r.relayEpisodes {
		e := &r.relayEpisodes[i]
		if e.final {
			continue
		}
		deadline := e.RestartStartNS + 2*relayloss.Budget
		if e.AppliedNS > 0 && e.AppliedNS <= deadline {
			var last time.Duration
			last, e.FirstVideoNS, e.FirstAudioNS = r.relayRecoveryPoints(e.RestartStartNS, e.AppliedNS, min(now, deadline))
			e.LastBeforeNS = max(e.LastBeforeNS, last)
		}
		e.final = r.hungUp || now >= deadline || e.FirstVideoNS > 0 && e.FirstAudioNS > 0
		if e.final && e.AppliedNS > 0 && e.AppliedNS <= deadline && r.historyStart <= e.LastBeforeNS {
			// Packet timestamps are taken before acquiring the recorder lock.
			// Settle the counts with the media evidence, not at answer application.
			for _, track := range r.tracks {
				row := RelayArrivalCount{Kind: track.kind, SSRC: track.ssrc}
				for _, arrival := range track.arrivals {
					if arrival > e.LastBeforeNS && arrival <= e.AppliedNS {
						row.Count++
					}
				}
				e.ArrivalsBeforeApplied = append(e.ArrivalsBeforeApplied, row)
			}
		}
	}
}

// RelayLossEpisodes returns independent copies. Each episode freezes on observed
// recovery or 2*Budget after its first attempt; late media cannot repair it.
func (c *Call) RelayLossEpisodes() []RelayRestartEpisode {
	r := c.rec
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observeRelayRestartsLocked(r.since(time.Now()))
	var out []RelayRestartEpisode
	for _, e := range r.relayEpisodes {
		row := e.RelayRestartEpisode
		row.ArrivalsBeforeApplied = slices.Clone(row.ArrivalsBeforeApplied)
		out = append(out, row)
	}
	return out
}
