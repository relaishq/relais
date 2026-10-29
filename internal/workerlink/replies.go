package workerlink

import (
	"time"

	"github.com/relaishq/relais/internal/privateapi"
)

// MaxReplyBytes bounds retained reply payloads per link, including queued and
// partially sent bodies. The payload and reply-count limits bound buffer storage.
const MaxReplyBytes = 4 * privateapi.MaxBody

// MaxQueuedReplies accommodates a completed handler generation and refusals
// for the next 64 callers without making handler saturation a link failure.
const MaxQueuedReplies = 2 * MaxConcurrent

func (p *Peer) reserveReply(n int) bool {
	p.replyMu.Lock()
	defer p.replyMu.Unlock()
	if p.replyClosed || n > MaxReplyBytes-p.replyBytes {
		return false
	}
	p.replyBytes += n
	return true
}
func (p *Peer) releaseReply(n int) { p.replyMu.Lock(); p.replyBytes -= n; p.replyMu.Unlock() }
func (p *Peer) queueReply(m Message) {
	p.replyMu.Lock()
	if p.replyClosed {
		p.replyBytes -= len(m.Body)
		p.replyMu.Unlock()
		return
	}
	select {
	case p.replySlots <- struct{}{}:
		p.replies <- m // Slots bound queued plus active messages, so this cannot block.
		p.replyMu.Unlock()
	default:
		p.replyBytes -= len(m.Body)
		p.replyMu.Unlock()
		// A peer that keeps sending without consuming 128 replies cannot be kept
		// indefinitely. Close with uncertainty rather than retain unbounded replies.
		p.Close()
	}
}
func (p *Peer) writeFrame(m Message, deadline time.Time) (Message, error) {
	p.writeMu.Lock()
	select {
	case <-p.done:
		p.writeMu.Unlock()
		return m, ErrUnavailable
	default:
	}
	if !deadline.IsZero() {
		m.Timeout = int64(time.Until(deadline))
	}
	err := Write(p.writer, m)
	p.writeMu.Unlock()
	if err != nil {
		p.Close()
	}
	return m, err
}
func (p *Peer) writeReplies() {
	type outgoing struct {
		message  Message
		reserved int
	}
	pending := make([]outgoing, 0, MaxConcurrent)
	finish := func(item outgoing) { p.releaseReply(item.reserved); <-p.replySlots }
	defer func() {
		for _, item := range pending {
			finish(item)
		}
		for {
			select {
			case m := <-p.replies:
				finish(outgoing{m, len(m.Body)})
			default:
				return
			}
		}
	}()
	for {
		if len(pending) == 0 {
			select {
			case <-p.done:
				return
			case m := <-p.replies:
				pending = append(pending, outgoing{m, len(m.Body)})
			}
		}
		select {
		case <-p.done:
			return
		default:
		}
	drain:
		for len(pending) < MaxConcurrent {
			select {
			case m := <-p.replies:
				pending = append(pending, outgoing{m, len(m.Body)})
			default:
				break drain
			}
		}
		item := pending[0]
		pending[0] = outgoing{}
		pending = pending[1:]
		chunk := item.message
		size := min(len(chunk.Body), ChunkSize)
		chunk.Body, chunk.More = chunk.Body[:size], len(chunk.Body) > size
		if _, err := p.writeFrame(chunk, time.Time{}); err != nil {
			finish(item)
			return
		}
		if chunk.More {
			item.message.Body = item.message.Body[size:]
			pending = append(pending, item)
		} else {
			finish(item)
		}
	}
}
