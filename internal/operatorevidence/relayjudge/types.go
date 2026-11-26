// Package relayjudge owns only the part R evidence contract and judgment.
// It is deliberately independent of judge-src and its release overlays.
package relayjudge

import (
	"encoding/json"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/operator"
)

// Input is supplied by the procedure after observation ends and restoration
// finishes. CallerJSONL and Samples are collected independently. Preflight is
// one control /status snapshot, translated to inventory host slots. No control
// field can substitute for caller media evidence.
type Input struct {
	Version            int               `json:"version"`
	RunID              string            `json:"runId"`
	Scenario           operator.Scenario `json:"scenario"`
	PID                int               `json:"pid"`
	Sessions           []string          `json:"sessions"` // caller ordinal order, exactly four
	Preflight          []Assignment      `json:"preflight"`
	TargetSlot         string            `json:"targetSlot"`
	SurvivorEndpoint   string            `json:"survivorEndpoint"`
	Restore            string            `json:"restore"` // restored, failed, or missing
	TargetInsufficient bool              `json:"targetInsufficient"`
	LostSessions       []string          `json:"lostSessions"` // positive control loss receipts
	CallerJSONL        string            `json:"callerJsonl"`
	Samples            []json.RawMessage `json:"samples"`
}
type Assignment struct {
	Session    string `json:"session"`
	Slot       string `json:"slot"`
	Generation uint64 `json:"generation"`
}
type Result struct {
	RunID   string       `json:"runId"`
	Outcome string       `json:"outcome"` // pass, fail, inconclusive
	Causes  []string     `json:"causes"`
	Calls   []CallResult `json:"calls"` // always four rows, including procedure failure
}
type CallResult struct {
	Ordinal int           `json:"ordinal"`
	Session string        `json:"session"`
	Role    string        `json:"role"` // affected, unaffected, or unknown
	Outcome string        `json:"outcome"`
	Causes  []string      `json:"causes"`
	Gap     time.Duration `json:"gapNs"`
}

// Decode only what R consumes. Pointers distinguish absent evidence from zero.
// The strict decoder still checks every unknown member for duplicate keys.
type message struct {
	invalid         bool // unambiguous malformed fields may retain independent failures
	recoveryInvalid bool
	extra           []message // duplicate records cannot erase independently valid failures

	Type     string                  `json:"type"`
	PID      int                     `json:"pid"`
	CallID   int                     `json:"call_id,omitempty"`
	Session  string                  `json:"session,omitempty"`
	Error    string                  `json:"error,omitempty"`
	Call     *report                 `json:"call,omitempty"`
	Dial     *callharness.DialReport `json:"dial,omitempty"`
	Stats    *loadgen.Stats          `json:"stats,omitempty"`
	Episodes []episode               `json:"relayLossEpisodes,omitempty"`
}
type report struct {
	StartedAt          *time.Time
	HungUpAt           *time.Duration
	FirstVideoSentAt   *time.Duration
	SentVideo          *struct{ Frames *int }
	ICERestarts        *int
	DecryptionFailures *struct{ AuthTag, Replay, Other *int }
	Tracks             []track
	Moves              []callharness.MoveReport
}
type track struct {
	Kind                      string
	SSRC                      *uint32
	Packets, DuplicatePackets *int
	MediaGap, LastArrival     *time.Duration
	Video                     *struct{ OnlineDecode *struct{ Errors *int } }
}
type episode struct {
	Start      *time.Duration `json:"restartStartNs"`
	Applied    *time.Duration `json:"appliedNs"`
	Generation *uint64        `json:"generation"`
	Candidate  *string        `json:"answerCandidate"`
	Last       *time.Duration `json:"lastBeforeNs"`
	Video      *time.Duration `json:"firstVideoNs"`
	Audio      *time.Duration `json:"firstAudioNs"`
	Counts     []arrival      `json:"arrivalsBeforeApplied"`
}
type arrival struct {
	Kind  string  `json:"kind"`
	SSRC  *uint32 `json:"ssrc"`
	Count *int    `json:"count"`
}
