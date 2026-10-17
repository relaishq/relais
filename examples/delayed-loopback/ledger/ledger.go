// Package ledger demonstrates a separate local, idempotent side-effect service.
// Keep this process and file alive when moving or killing media workers. This is
// a bounded teaching service, not a provider transaction or production database.
package ledger

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/workerapp"
)

const Name = "local-ledger/v1"
const TaskKind = "local-ledger/commit/v1"
const maxEntries = 4096

type entry struct {
	Version int    `json:"version"`
	Key     string `json:"key"`
	Value   []byte `json:"value"`
	Number  int    `json:"number"`
}

// Ledger serializes commits and deduplicates by the host-derived logical task
// key. Sync precedes every first acknowledgement. A write error poisons this
// process: it cannot know whether bytes reached the file, so it stops accepting.
type Ledger struct {
	mu         sync.Mutex
	file       *os.File
	entries    map[string]entry
	poisoned   bool
	deliveries int
}

func Open(path string) (*Ledger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	// Every canonical append ends with a newline. A partial final write can
	// otherwise be valid JSON but would concatenate with the next append.
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if info.Size() > 0 {
		var tail [1]byte
		if _, err := f.ReadAt(tail[:], info.Size()-1); err != nil {
			_ = f.Close()
			return nil, err
		}
		if tail[0] != '\n' {
			_ = f.Close()
			return nil, errors.New("ledger: unterminated record")
		}
	}
	l := &Ledger{file: f, entries: make(map[string]entry)}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var e entry
		if err := decodeEntry(scanner.Bytes(), &e); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("ledger: malformed record: %w", err)
		}
		if e.Version != 2 || e.Key == "" || e.Number != len(l.entries)+1 || len(l.entries) >= maxEntries {
			_ = f.Close()
			return nil, errors.New("ledger: invalid record")
		}
		if _, ok := l.entries[e.Key]; ok {
			_ = f.Close()
			return nil, errors.New("ledger: duplicate record")
		}
		l.entries[e.Key] = e
	}
	if err := scanner.Err(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return l, nil
}

// Version 2 stores bytes as JSON base64. Read version 1's text records for
// existing example files; decoding never treats old text as base64.
func decodeEntry(data []byte, e *entry) error {
	var wire struct {
		Version int             `json:"version"`
		Key     string          `json:"key"`
		Value   json.RawMessage `json:"value"`
		Number  int             `json:"number"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*e = entry{Version: 2, Key: wire.Key, Number: wire.Number}
	switch wire.Version {
	case 1:
		var text string
		if err := json.Unmarshal(wire.Value, &text); err != nil {
			return err
		}
		e.Value = []byte(text)
	case 2:
		if err := json.Unmarshal(wire.Value, &e.Value); err != nil {
			return err
		}
	default:
		return errors.New("ledger: unknown record version")
	}
	return nil
}

func (l *Ledger) Close() error { return l.file.Close() }

// Counts is local test/diagnostic evidence: delivery can exceed commit count.
func (l *Ledger) Counts() (deliveries, commits int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.deliveries, len(l.entries)
}
func (l *Ledger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/commit" {
		http.NotFound(w, r)
		return
	}
	var incoming struct {
		Key   string `json:"key"`
		Value []byte `json:"value"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&incoming) != nil || decoder.Decode(new(any)) != io.EOF || incoming.Key == "" || len(incoming.Key) > 256 || len(incoming.Value) > 512 {
		http.Error(w, "invalid request", 400)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.deliveries++
	if l.poisoned {
		http.Error(w, "uncertain ledger write; restart and inspect file", http.StatusServiceUnavailable)
		return
	}
	e, exists := l.entries[incoming.Key]
	if exists && !bytes.Equal(e.Value, incoming.Value) {
		http.Error(w, "idempotency conflict", http.StatusConflict)
		return
	}
	if !exists {
		if len(l.entries) >= maxEntries {
			http.Error(w, "ledger capacity", http.StatusInsufficientStorage)
			return
		}
		e = entry{Version: 2, Key: incoming.Key, Value: incoming.Value, Number: len(l.entries) + 1}
		data, err := json.Marshal(e)
		if err == nil {
			data = append(data, '\n')
			var n int
			n, err = l.file.Write(data)
			if err == nil && n != len(data) {
				err = io.ErrShortWrite
			}
		}
		if err == nil {
			err = l.file.Sync()
		}
		if err != nil {
			l.poisoned = true
			http.Error(w, "uncertain ledger write", http.StatusServiceUnavailable)
			return
		}
		l.entries[e.Key] = e
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(e)
}

func localURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return nil, errors.New("ledger: require loopback HTTP origin")
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil || port == "0" {
		return nil, errors.New("ledger: require loopback address and port")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("ledger: require literal loopback IP")
	}
	return u, nil
}
func Handler(origin string) agent.TaskHandler {
	return func(ctx context.Context, run *agent.TaskRun) error {
		u, err := localURL(origin)
		if err != nil {
			return run.Fail(agent.Failed, err.Error())
		}
		request := run.Spec().Request
		data, err := json.Marshal(struct {
			Key   string `json:"key"`
			Value []byte `json:"value"`
		}{run.Key(), request})
		if err != nil {
			return err
		}
		// A task attempt gets at most two deliveries. Both use the same key,
		// immutable request bytes, and parent deadline; cancellation stops retry.
		client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		defer client.CloseIdleConnections()
		var reply []byte
		for attempt := 0; attempt < 2; attempt++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			var outcome agent.Outcome
			reply, outcome, err = commitAttempt(ctx, client, u.String()+"/commit", data, run.Key(), request)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if outcome != agent.OutcomeUnknown || attempt == 1 {
				return run.Fail(outcome, err.Error())
			}
		}
		if run.After() < 1 {
			return run.Emit(ctx, 1, agent.Chunk{Data: reply})
		}
		return nil
	}
}

// commitAttempt owns and closes one HTTP response before another delivery.
func commitAttempt(ctx context.Context, client *http.Client, endpoint string, data []byte, key string, request []byte) ([]byte, agent.Outcome, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, agent.Failed, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return nil, agent.OutcomeUnknown, errors.New("ledger delivery has no verified reply")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusInsufficientStorage {
		return nil, agent.Capacity, errors.New("ledger is full")
	}
	if response.StatusCode != http.StatusOK {
		if response.StatusCode >= 400 && response.StatusCode < 500 {
			return nil, agent.Failed, errors.New("ledger refused commit")
		}
		return nil, agent.OutcomeUnknown, errors.New("ledger commit is unverified")
	}
	reply, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil {
		return nil, agent.OutcomeUnknown, errors.New("ledger receipt was lost")
	}
	var receipt entry
	if len(reply) > 4096 || json.Unmarshal(reply, &receipt) != nil || receipt.Version != 2 || receipt.Key != key || !bytes.Equal(receipt.Value, request) {
		return nil, agent.OutcomeUnknown, errors.New("unverified ledger receipt")
	}
	return reply, agent.OK, nil
}

type demoAgent struct {
	Cursor  uint64
	Outcome agent.Outcome
}

func (a *demoAgent) Handle(_ context.Context, e agent.Event) (agent.Effects, error) {
	switch e.Kind {
	case agent.Started:
		return agent.Effects{StartTasks: []agent.TaskSpec{{Step: "commit", Kind: TaskKind, Turn: 1, Class: agent.SideEffecting, Resume: agent.Retry, Request: []byte("one demonstration commit"), Timeout: 10 * time.Second}}}, nil
	case agent.TaskMessage:
		if e.Result != nil {
			a.Cursor = e.Result.Sequence
			a.Outcome = e.Result.Outcome
		}
	}
	return agent.Effects{}, nil
}
func (a *demoAgent) Save(context.Context) (agent.State, error) {
	b, err := json.Marshal(a)
	return agent.State{Version: 1, Bytes: b}, err
}
func (a *demoAgent) Restore(_ context.Context, s agent.State) error {
	if s.Version != 1 {
		return agent.ErrVersion
	}
	return json.Unmarshal(s.Bytes, a)
}
func Registration(origin string) workerapp.Registration {
	return workerapp.Registration{Name: Name, Factory: func(agent.FactoryArgs) agent.Agent { return &demoAgent{} }, Tasks: map[string]agent.TaskHandler{TaskKind: Handler(origin)}}
}

// Main runs only the local teaching ledger, separate from the worker process.
func Main(args []string) {
	flags := flag.NewFlagSet("delayed-loopback ledger", flag.ExitOnError)
	listen := flags.String("listen", "127.0.0.1:0", "loopback HTTP address")
	path := flags.String("file", "", "new or existing local ledger file (one server per file)")
	_ = flags.Parse(args)
	host, _, err := net.SplitHostPort(*listen)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() || strings.TrimSpace(*path) == "" {
		log.Fatal("ledger requires -file and literal loopback -listen")
	}
	l, err := Open(*path)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Handler: l, ReadHeaderTimeout: time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	fmt.Printf("ledger_url=http://%s\n", listener.Addr())
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
