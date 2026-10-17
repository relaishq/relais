package voice

// Window remembers sparse extended RTP indexes, including gaps and sequence
// wraps. Older indexes fall below a monotonic floor and cannot be recorded again.
type Window struct {
	SSRC uint32    `json:"ssrc"`
	High uint64    `json:"high"`
	Mask [2]uint64 `json:"mask"`
	Set  bool      `json:"set"`
}

func (w *Window) Accept(ssrc uint32, index uint64) bool {
	if !w.Set {
		*w = Window{SSRC: ssrc, High: index, Mask: [2]uint64{1, 0}, Set: true}
		return true
	}
	// One negotiated audio source per call. Do not mistake a different source
	// for a duplicate of the current one.
	if w.SSRC != ssrc {
		return false
	}
	if index > w.High {
		delta := index - w.High
		switch {
		case delta >= 128:
			w.Mask = [2]uint64{}
		case delta >= 64:
			w.Mask = [2]uint64{0, w.Mask[0] << (delta - 64)}
		default:
			w.Mask = [2]uint64{w.Mask[0] << delta, w.Mask[1]<<delta | w.Mask[0]>>(64-delta)}
		}
		w.High = index
	}
	delta := w.High - index
	if delta >= 128 {
		return false
	}
	word, bit := delta/64, uint(delta%64)
	if w.Mask[word]&(uint64(1)<<bit) != 0 {
		return false
	}
	w.Mask[word] |= uint64(1) << bit
	return true
}
