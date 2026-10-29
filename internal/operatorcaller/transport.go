package operatorcaller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/operator"
)

// MailboxPeer is implemented by the existing framed pipe and subprocess adapter.
// The adapter serves only relative mailbox keys; it never implements feed logic.
type MailboxPeer interface {
	Send(context.Context, operator.Frame) error
	Receive(context.Context) (operator.Frame, error)
}
type ProtocolMailbox struct {
	Peer    MailboxPeer
	Session string
	Spool   *os.Root
	End     time.Time
	mu      sync.Mutex
	next    uint64
	broken  bool
}
type MailboxArgs struct {
	Key  string         `json:"key"`
	Data []byte         `json:"data,omitempty"`
	Blob *operator.Blob `json:"blob,omitempty"`
}
type MailboxValue struct {
	Found *bool          `json:"found,omitempty"`
	Data  []byte         `json:"data,omitempty"`
	Blob  *operator.Blob `json:"blob,omitempty"`
	State CreateState    `json:"state,omitempty"`
}

func (m *ProtocolMailbox) exchange(ctx context.Context, op operator.Operation, key string, data []byte) (MailboxValue, error) {
	var value MailboxValue
	if m.broken || !validKey(key) || len(data) > operator.MaxBlobBytes {
		return value, errors.New("mailbox-unavailable")
	}
	if m.next >= maxSafeInteger {
		m.broken = true
		return value, errors.New("mailbox-id-exhausted")
	}
	m.next++
	id := strconv.FormatUint(m.next, 10)
	args := MailboxArgs{Key: key, Data: data}
	if len(data) > operator.MaxFrameBytes/4 {
		if m.Spool == nil {
			return value, errors.New("mailbox-spool-required")
		}
		scope := sha256.Sum256([]byte(m.Session + "\x00" + key))
		name := "caller-put-" + hex.EncodeToString(scope[:]) + "-" + id
		sum := sha256.Sum256(data)
		f, err := m.Spool.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return value, err
		}
		_, writeErr := f.Write(data)
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		if err = errors.Join(writeErr, closeErr); err != nil {
			return value, err
		}
		args.Blob = &operator.Blob{Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
		args.Data = nil
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return value, err
	}
	end := minTime(m.End, time.Now().Add(10*time.Second))
	if deadline, ok := ctx.Deadline(); ok {
		end = minTime(end, deadline)
	}
	ctx, cancel := context.WithDeadline(ctx, end)
	defer cancel()
	r := operator.Request{ID: id, Operation: op, Target: "caller", Args: encoded, DeadlineUnixMS: strconv.FormatInt(end.UnixMilli(), 10), RemainingMS: max(0, time.Until(end).Milliseconds())}
	if err = m.Peer.Send(ctx, operator.Frame{Type: "request", Session: m.Session, Request: &r}); err != nil {
		m.broken = true
		return value, err
	}
	reply, err := m.Peer.Receive(ctx)
	if err != nil || ctx.Err() != nil || reply.Type != "result" || reply.Session != m.Session || reply.Result == nil || reply.Result.ID != id || reply.Result.Transport != operator.Succeeded || reply.Result.ExitCode == nil || *reply.Result.ExitCode != 0 {
		m.broken = true
		return value, errors.New("mailbox-result-unknown")
	}
	resolved, err := operatortransport.ResolveReply(m.Spool, *reply.Result)
	if err != nil || decode(resolved.Payload, &value) != nil {
		m.broken = true
		return value, errors.New("invalid-mailbox-result")
	}
	return value, nil
}
func (m *ProtocolMailbox) Read(ctx context.Context, key string) (ReadResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, err := m.exchange(ctx, operator.MailboxRead, key, nil)
	if err != nil {
		return ReadResult{}, err
	}
	if value.Found == nil || !*value.Found && (len(value.Data) > 0 || value.Blob != nil) {
		return ReadResult{}, errors.New("missing-mailbox-presence")
	}
	if value.Blob != nil {
		if len(value.Data) > 0 || m.Spool == nil {
			return ReadResult{}, errors.New("invalid-mailbox-blob")
		}
		value.Data, err = operatortransport.ReadBlob(m.Spool, *value.Blob, operator.MaxBlobBytes)
		if err != nil {
			return ReadResult{}, err
		}
	}
	return ReadResult{Found: *value.Found, Data: value.Data}, nil
}
func (m *ProtocolMailbox) Create(ctx context.Context, key string, data []byte) CreateState {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, err := m.exchange(ctx, operator.MailboxCreate, key, data)
	if err != nil {
		return CreateUnknown
	}
	if value.State == Committed || value.State == Conflict {
		return value.State
	}
	m.broken = true
	return CreateUnknown
}
