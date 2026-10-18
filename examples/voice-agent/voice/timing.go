package voice

import (
	"time"
)

// Timings are nanosecond offsets from accepted release, on the host store clock.
// Zero means unavailable except RecordingClosed: recording closes in the stop
// callback, so that span is zero. Completed values are never recomputed on move.
// Stage spans run from the callback starting work to its result callback and
// include admission and result delivery. Backward clock steps floor durations at
// zero. Queued means host audio admission,
// not decoded audio or human hearing.
type Timings struct {
	RecordingClosed         int64 `json:"recording_closed_ns,omitempty"`
	TranscribeStarted       int64 `json:"transcribe_started_ns,omitempty"`
	TranscribeFinished      int64 `json:"transcribe_finished_ns,omitempty"`
	ReplyStarted            int64 `json:"reply_started_ns,omitempty"`
	ReplyFinished           int64 `json:"reply_finished_ns,omitempty"`
	ReplyCharacters         int   `json:"reply_characters,omitempty"`
	FirstSentenceCharacters int   `json:"first_sentence_characters,omitempty"`
	SpeechStarted           int64 `json:"speech_started_ns,omitempty"`
	SpeechFinished          int64 `json:"speech_finished_ns,omitempty"`
	SpeechAudio             int64 `json:"speech_audio_ns,omitempty"`
	FirstQueued             int64 `json:"first_queued_ns,omitempty"`
}

func (a *Agent) offset(at time.Time) int64 {
	if at.IsZero() || a.state.ReleaseAt == 0 {
		return 0
	}
	return max(0, at.UnixNano()-a.state.ReleaseAt)
}
