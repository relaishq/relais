// Package operatorcaller bridges the unchanged relais-load process to a scoped
// mailbox. Provider delivery and credentials belong to adapters.
package operatorcaller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/relaishq/relais/pkg/operator"
)

type CreateState string

const (
	Committed     CreateState = "committed"
	Conflict      CreateState = "conflict"
	CreateUnknown CreateState = "unknown"
)

type ReadResult struct {
	Found bool
	Data  []byte
}
type Store interface {
	Read(context.Context, string) (ReadResult, error)
	Create(context.Context, string, []byte) CreateState
}

// Mailbox owns conductor action reservations. Share one instance across the
// brokers and launcher; adapters implement Store and cannot reset reservations.
type Mailbox struct {
	Store
	mu      sync.Mutex
	hangups map[string]CreateState
}

func NewMailbox(store Store) *Mailbox { return &Mailbox{Store: store} }
func (m *Mailbox) hangup(ctx context.Context, key string, value any) CreateState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state, exists := m.hangups[key]; exists {
		return state
	}
	if m.hangups == nil {
		m.hangups = map[string]CreateState{}
	}
	// Reserve before crossing the delivery boundary. Unknown is terminal.
	m.hangups[key] = CreateUnknown
	state := createJSON(ctx, m.Store, key, value)
	m.hangups[key] = state
	return state
}

// Directory uses an opened root and atomically links a fully written temporary
// object into place. Readers never see a partially published object. Its owner
// must exclusively control this directory; conflicts never prove equal content.
type Directory struct{ root *os.Root }

func OpenDirectory(directory string) (*Directory, error) {
	r, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	return &Directory{root: r}, nil
}
func (d *Directory) Close() error { return d.root.Close() }
func validKey(key string) bool {
	return fs.ValidPath(key) && key != "." && !strings.ContainsAny(key, "\\\x00") && len(key) <= 512
}
func (d *Directory) Read(ctx context.Context, key string) (ReadResult, error) {
	if !validKey(key) {
		return ReadResult{}, errors.New("invalid-mailbox-key")
	}
	if err := ctx.Err(); err != nil {
		return ReadResult{}, err
	}
	f, err := d.root.Open(key)
	if errors.Is(err, fs.ErrNotExist) {
		return ReadResult{}, nil
	}
	if err != nil {
		return ReadResult{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, operator.MaxBlobBytes+1))
	if err != nil {
		return ReadResult{}, err
	}
	if len(b) > operator.MaxBlobBytes {
		return ReadResult{}, errors.New("mailbox-size")
	}
	return ReadResult{Found: true, Data: b}, nil
}
func (d *Directory) Create(ctx context.Context, key string, data []byte) CreateState {
	if !validKey(key) || len(data) > operator.MaxBlobBytes || ctx.Err() != nil {
		return CreateUnknown
	}
	dir := path.Dir(key)
	if err := d.root.MkdirAll(dir, 0700); err != nil {
		return CreateUnknown
	}
	// Random names are local staging details, never wire identities.
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return CreateUnknown
	}
	name := ".mailbox-" + hex.EncodeToString(nonce)
	f, err := d.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return CreateUnknown
	}
	defer func() { _ = d.root.Remove(name) }()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil || ctx.Err() != nil {
		return CreateUnknown
	}
	err = d.root.Link(name, key)
	if errors.Is(err, fs.ErrExist) {
		return Conflict
	}
	if err != nil {
		return CreateUnknown
	}
	return Committed
}
