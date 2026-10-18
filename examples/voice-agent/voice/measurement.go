package voice

import (
	"fmt"
	"io"
	"slices"
	"time"
)

// Measurement contains numbers only. FirstAudio is measured at the Pion client;
// FirstQueued is the independent host callback boundary.
type Measurement struct {
	WarmMode   string
	Worker     int
	Turn       int
	Timings    Timings
	FirstAudio time.Duration
	Requests   []RequestTrace
}

// WriteMeasurementTable repeats a turn's stage offsets/spans for each request,
// including warm and retries. It never includes text, audio, URLs or credentials.
func WriteMeasurementTable(w io.Writer, rows []Measurement) error {
	_, err := fmt.Fprintln(w, "worker\tturn\twarm_mode\tidle_before_s\trecord_closed_ms\tstt_start_ms\tstt_end_ms\tstt_ms\treply_start_ms\treply_end_ms\treply_ms\treply_chars\tfirst_sentence_chars\tspeech0_start_ms\tspeech0_end_ms\tspeech0_ms\tspeech0_audio_ms\tfirst_queued_ms\tpion_first_audio_ms\trequest_turn\tstage\tdns_ms\tconnect_ms\ttls_ms\treused\tidle\tidle_ms\tthink_ms\tbody_ms\tprotocol\tstatus")
	if err != nil {
		return err
	}
	ms := func(n int64) float64 { return float64(n) / float64(time.Millisecond) }
	for _, row := range rows {
		t := row.Timings
		for _, r := range row.Requests {
			_, err = fmt.Fprintf(w, "%d\t%d\t%s\t%s\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%d\t%d\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%d\t%s\t%.3f\t%.3f\t%.3f\t%t\t%t\t%.3f\t%.3f\t%.3f\t%s\t%d\n", row.Worker, row.Turn, normalizeWarmMode(row.WarmMode), r.idleBefore, ms(t.RecordingClosed), ms(t.TranscribeStarted), ms(t.TranscribeFinished), ms(max(0, t.TranscribeFinished-t.TranscribeStarted)), ms(t.ReplyStarted), ms(t.ReplyFinished), ms(max(0, t.ReplyFinished-t.ReplyStarted)), t.ReplyCharacters, t.FirstSentenceCharacters, ms(t.SpeechStarted), ms(t.SpeechFinished), ms(max(0, t.SpeechFinished-t.SpeechStarted)), ms(t.SpeechAudio), ms(t.FirstQueued), ms(int64(row.FirstAudio)), r.Turn, r.Stage, ms(int64(r.DNS)), ms(int64(r.Connect)), ms(int64(r.TLS)), r.Reused, r.Idle, ms(int64(r.IdleTime)), ms(int64(r.Think)), ms(int64(r.BodyRead)), r.Protocol, r.Status)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// RequestHistory measures process-local idle time from the prior request's body
// completion to the next request start. Overlapping requests have zero idle.
// Keep one history across all workers in a measurement process.
type RequestHistory struct{ finished time.Time }

func (h *RequestHistory) Observe(requests []RequestTrace) []RequestTrace {
	rows := slices.Clone(requests)
	slices.SortStableFunc(rows, func(a, b RequestTrace) int { return a.started.Compare(b.started) })
	for i := range rows {
		r := &rows[i]
		r.idleBefore = "first"
		if !h.finished.IsZero() {
			r.idleBefore = fmt.Sprintf("%.3f", max(0, r.started.Sub(h.finished).Seconds()))
		}
		if r.finished.After(h.finished) {
			h.finished = r.finished
		}
	}
	return rows
}
