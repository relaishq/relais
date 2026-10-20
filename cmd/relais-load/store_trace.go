package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This opt-in plaintext observer is restricted to the driver's disposable
// loopback Redis. It forwards exact RESP frames and stores sizes, never values.
// INFO/cleanup/inventory use the real Redis address and bypass this observer.
type wireValue struct {
	raw   []byte
	text  []byte
	array []wireValue
	kind  byte
}

func readWire(r *bufio.Reader, depth int) (v wireValue, err error) {
	if depth > 16 {
		return v, errors.New("RESP nesting exceeds observer bound")
	}
	line, err := r.ReadBytes('\n')
	if err != nil {
		return v, err
	}
	if len(line) < 3 || line[len(line)-2] != '\r' {
		return v, errors.New("invalid RESP line")
	}
	v.kind = line[0]
	v.raw = line
	v.text = line[1 : len(line)-2]
	switch v.kind {
	case '+', '-', ':':
		return v, nil
	case '$', '*':
		n, err := strconv.Atoi(string(v.text))
		if err != nil || n < -1 || n > 16<<20 {
			return v, errors.New("invalid RESP size")
		}
		if n == -1 {
			return v, nil
		}
		if v.kind == '$' {
			body := make([]byte, n+2)
			if _, err := io.ReadFull(r, body); err != nil {
				return v, err
			}
			if !bytes.HasSuffix(body, []byte("\r\n")) {
				return v, errors.New("invalid RESP bulk ending")
			}
			v.text = body[:n]
			v.raw = append(v.raw, body...)
		} else {
			if n > 65536 {
				return v, errors.New("RESP array exceeds observer bound")
			}
			for range n {
				child, err := readWire(r, depth+1)
				if err != nil {
					return v, err
				}
				v.array = append(v.array, child)
				v.raw = append(v.raw, child.raw...)
			}
		}
		return v, nil
	default:
		return v, errors.New("unsupported RESP frame")
	}
}

type byteSizes struct {
	Writes int   `json:"writes"`
	Bytes  int64 `json:"bytes"`
	Min    int   `json:"min_bytes"`
	Max    int   `json:"max_bytes"`
}

func (s *byteSizes) add(n int) {
	s.Writes++
	s.Bytes += int64(n)
	if s.Writes == 1 || n < s.Min {
		s.Min = n
	}
	s.Max = max(s.Max, n)
}

type wireWindow struct {
	ScriptBodies         int64                 `json:"script_body_submissions"`
	NoScriptReplies      int64                 `json:"noscript_replies"`
	Transactions         map[string]*byteSizes `json:"transaction_payloads"`
	HSET                 map[string]*byteSizes `json:"hset_field_values"`
	HSETFieldNames       map[string]*byteSizes `json:"hset_field_names"`
	KeyExamples          []string              `json:"key_examples,omitempty"`
	Commands             int64                 `json:"commands"`
	RequestBytes         int64                 `json:"request_bytes"`
	ReplyBytes           int64                 `json:"reply_bytes"`
	ScriptLoads          int64                 `json:"script_loads"`
	WriteAttempts        int64                 `json:"write_attempts"`
	WriteFailures        int64                 `json:"write_failures"`
	Components           map[string]*byteSizes `json:"components"`
	KeyArguments         map[string]*byteSizes `json:"key_arguments"`
	KeyRESPBytes         int64                 `json:"key_resp_bytes"`
	NumkeysRESPBytes     int64                 `json:"numkeys_resp_bytes"`
	CheckpointHSETFields map[string]*byteSizes `json:"checkpoint_hset_fields"`
	CommitLatencyMS      []float64             `json:"commit_request_reply_ms,omitempty"`
}
type storeTrace struct {
	PhaseSeconds map[string]float64     `json:"phase_seconds"`
	Phases       map[string]*wireWindow `json:"phases"`
	Seconds      map[int64]*wireWindow  `json:"seconds"`
	Errors       []string               `json:"errors,omitempty"`
	Note         string                 `json:"note"`
}
type storeObserver struct {
	phaseAt  time.Time
	mu       sync.Mutex
	listener net.Listener
	backend  string
	phase    string
	started  time.Time
	trace    storeTrace
	conns    map[net.Conn]bool
	closed   bool
	wg       sync.WaitGroup
}

func startStoreObserver(backend string) (*storeObserver, error) {
	host, _, err := net.SplitHostPort(backend)
	if err != nil || host != "127.0.0.1" {
		return nil, errors.New("store observer requires owned loopback Redis")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	o := &storeObserver{listener: listener, backend: backend, phase: "setup", started: time.Now(), conns: map[net.Conn]bool{}, phaseAt: time.Now(), trace: storeTrace{PhaseSeconds: map[string]float64{}, Phases: map[string]*wireWindow{}, Seconds: map[int64]*wireWindow{}, Note: "Exact plaintext RESP request/reply sizes at an observing proxy on one runner. A separate MONITOR stream counts actual Lua HSET field/value lengths, including initialization and lease metadata. MONITOR overhead is outside application traffic but consumes this same runner. Both arms use the same observer; forwarding adds overhead. Client metrics remain authoritative by session/frame/owners. Script body submissions count every EVAL/SCRIPT LOAD, including warm repeated EVAL bodies. Script loads count actual SCRIPT LOAD commands; NOSCRIPT replies are separate. No Redis internal compilation/cache-hit count is inferred from a warm EVAL submission. Successful v10 commit arguments supply ciphertext and packed metadata lengths; HSET b/m/t field names are one byte each and t is the frozen eight-byte store-clock field. No plaintext or ciphertext values are retained. Failed/unknown replies count attempts but no successful component write. No TLS/NIC/log bytes measured."}}
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			o.mu.Lock()
			if o.closed {
				o.mu.Unlock()
				_ = c.Close()
				return
			}
			o.conns[c] = true
			o.wg.Add(1)
			o.mu.Unlock()
			go o.serve(c)
		}
	}()
	if err := o.startMonitor(); err != nil {
		o.close()
		return nil, err
	}
	return o, nil
}
func (o *storeObserver) mark(phase string) {
	o.mu.Lock()
	o.trace.PhaseSeconds[o.phase] += time.Since(o.phaseAt).Seconds()
	o.phaseAt = time.Now()
	o.phase = phase
	o.mu.Unlock()
}
func (o *storeObserver) serve(c net.Conn) {
	defer o.wg.Done()
	defer func() { _ = c.Close(); o.mu.Lock(); delete(o.conns, c); o.mu.Unlock() }()
	backend, err := net.DialTimeout("tcp4", o.backend, time.Second)
	if err != nil {
		o.problem(err)
		return
	}
	defer func() { _ = backend.Close() }()
	o.mu.Lock()
	o.conns[backend] = true
	o.mu.Unlock()
	defer func() { o.mu.Lock(); delete(o.conns, backend); o.mu.Unlock() }()
	requests, replies := bufio.NewReader(c), bufio.NewReader(backend)
	for {
		req, err := readWire(requests, 0)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				o.problem(err)
			}
			return
		}
		o.mu.Lock()
		phase := o.phase
		second := int64(time.Since(o.started).Seconds())
		o.mu.Unlock()
		started := time.Now()
		_, writeErr := backend.Write(req.raw)
		var reply wireValue
		if writeErr == nil {
			reply, err = readWire(replies, 0)
		} else {
			err = writeErr
		}
		o.mu.Lock()
		if o.trace.Phases[phase] == nil {
			o.trace.Phases[phase] = newWireWindow()
		}
		if o.trace.Seconds[second] == nil {
			o.trace.Seconds[second] = newWireWindow()
		}
		for _, window := range []*wireWindow{o.trace.Phases[phase], o.trace.Seconds[second]} {
			window.observe(req, reply, err, time.Since(started))
		}
		o.mu.Unlock()
		if err != nil {
			o.problem(err)
			return
		}
		if _, err = c.Write(reply.raw); err != nil {
			return
		}
	}
}
func (o *storeObserver) problem(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.closed && len(o.trace.Errors) < 32 {
		o.trace.Errors = append(o.trace.Errors, err.Error())
	}
}
func (o *storeObserver) close() storeTrace {
	o.mu.Lock()
	if !o.closed {
		o.trace.PhaseSeconds[o.phase] += time.Since(o.phaseAt).Seconds()
	}
	o.closed = true
	_ = o.listener.Close()
	for c := range o.conns {
		_ = c.Close()
	}
	o.mu.Unlock()
	o.wg.Wait()
	return o.trace
}
func newWireWindow() *wireWindow {
	return &wireWindow{Transactions: map[string]*byteSizes{}, HSET: map[string]*byteSizes{}, HSETFieldNames: map[string]*byteSizes{}, Components: map[string]*byteSizes{}, KeyArguments: map[string]*byteSizes{}, CheckpointHSETFields: map[string]*byteSizes{}}
}
func addSize(values map[string]*byteSizes, key string, n int) {
	if values[key] == nil {
		values[key] = &byteSizes{}
	}
	values[key].add(n)
}
func (w *wireWindow) observe(req, reply wireValue, err error, elapsed time.Duration) {
	w.Commands++
	w.RequestBytes += int64(len(req.raw))
	w.ReplyBytes += int64(len(reply.raw))
	a := req.array
	if len(a) == 0 {
		return
	}
	command := strings.ToLower(string(a[0].text))
	if reply.kind == '-' && strings.HasPrefix(string(reply.text), "NOSCRIPT") {
		w.NoScriptReplies++
	}
	if command == "script" && len(a) > 1 && strings.EqualFold(string(a[1].text), "load") {
		w.ScriptBodies++
		w.ScriptLoads++
	}
	if command != "eval" && command != "evalsha" {
		return
	}
	if command == "eval" {
		w.ScriptBodies++
	}
	if len(a) < 3 {
		return
	}
	n, e := strconv.Atoi(string(a[2].text))
	if e != nil || n < 1 || len(a) < 3+n {
		return
	}
	args := a[3+n:]
	split := n == 11 && len(args) == 11 && strings.Contains(string(a[9].text), "checkpoint:")
	legacy := len(args) == 8 && string(args[0].text) == "put"
	if !split && !legacy {
		return
	}
	w.WriteAttempts++
	if split {
		payload := len(args[4].text) + len(args[3].text) + 8 + 3 + len(args[7].text) + len(args[8].text) + len(args[9].text)
		addSize(w.Transactions, "attempted_total", payload)
	}
	w.KeyRESPBytes += int64(len(a[2].raw))
	w.NumkeysRESPBytes += int64(len(a[2].raw))
	if len(w.KeyExamples) == 0 {
		for _, key := range a[3 : 3+n] {
			w.KeyExamples = append(w.KeyExamples, string(key.text))
		}
	}
	for i, key := range a[3 : 3+n] {
		addSize(w.KeyArguments, fmt.Sprintf("%02d", i+1), len(key.text))
		w.KeyRESPBytes += int64(len(key.raw))
	}
	if err != nil || len(reply.array) == 0 || string(reply.array[0].text) != "1" {
		w.WriteFailures++
		return
	}
	w.CommitLatencyMS = append(w.CommitLatencyMS, float64(elapsed)/float64(time.Millisecond))
	if legacy {
		addSize(w.Components, "legacy", len(args[5].text))
		return
	}
	hot := len(args[4].text) + len(args[3].text) + 8 + 3 + len(args[8].text)
	addSize(w.Transactions, "published_hot", hot)
	addSize(w.Transactions, "published_total", hot+len(args[7].text)+len(args[9].text))
	addSize(w.Components, "checkpoint", len(args[4].text))
	for i, name := range []string{"setup", "work", "history"} {
		if len(args[7+i].text) > 0 {
			addSize(w.Components, name, len(args[7+i].text))
		}
	}
	for name, size := range map[string]int{"b": len(args[4].text), "m": len(args[3].text), "t": 8} {
		addSize(w.CheckpointHSETFields, name, size)
		addSize(w.CheckpointHSETFields, "field_names", 1)
	}
}

// MONITOR observes actual Lua HSET calls, including initialization and lease
// metadata. Only field/value lengths survive; bytes never enter artifacts.
func (o *storeObserver) startMonitor() error {
	conn, err := net.DialTimeout("tcp4", o.backend, time.Second)
	if err != nil {
		return err
	}
	if _, err = conn.Write([]byte("*1\r\n$7\r\nMONITOR\r\n")); err != nil {
		_ = conn.Close()
		return err
	}
	reader := bufio.NewReader(conn)
	ack, err := readWire(reader, 0)
	if err != nil || string(ack.text) != "OK" {
		_ = conn.Close()
		return errors.New("redis MONITOR unavailable")
	}
	o.mu.Lock()
	o.conns[conn] = true
	o.wg.Add(1)
	o.mu.Unlock()
	go func() {
		defer o.wg.Done()
		defer func() { _ = conn.Close(); o.mu.Lock(); delete(o.conns, conn); o.mu.Unlock() }()
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					o.problem(err)
				}
				return
			}
			args, err := monitorArguments(line)
			if err != nil {
				o.problem(err)
				continue
			}
			if len(args) < 4 || !strings.EqualFold(args[0], "hset") {
				continue
			}
			family := args[1]
			if i := strings.Index(family, ":{sess:"); i >= 0 {
				family = family[:i]
			}
			if i := strings.LastIndex(family, ":"); i >= 0 {
				family = family[i+1:]
			}
			o.mu.Lock()
			phase := o.phase
			second := int64(time.Since(o.started).Seconds())
			if o.trace.Phases[phase] == nil {
				o.trace.Phases[phase] = newWireWindow()
			}
			if o.trace.Seconds[second] == nil {
				o.trace.Seconds[second] = newWireWindow()
			}
			for i := 2; i+1 < len(args); i += 2 {
				for _, w := range []*wireWindow{o.trace.Phases[phase], o.trace.Seconds[second]} {
					key := family + ":" + args[i]
					addSize(w.HSET, key, len(args[i+1]))
					addSize(w.HSETFieldNames, key, len(args[i]))
				}
			}
			o.mu.Unlock()
		}
	}()
	return nil
}
func monitorArguments(line string) ([]string, error) {
	_, rest, ok := strings.Cut(line, "] ")
	if !ok {
		return nil, errors.New("malformed MONITOR prefix")
	}
	var args []string
	for rest = strings.TrimSpace(rest); rest != ""; rest = strings.TrimSpace(rest) {
		if rest[0] != '"' {
			return nil, errors.New("malformed MONITOR argument")
		}
		end := 1
		for end < len(rest) {
			if rest[end] == '\\' {
				end += 2
				continue
			}
			if rest[end] == '"' {
				break
			}
			end++
		}
		if end >= len(rest) {
			return nil, errors.New("unterminated MONITOR argument")
		}
		value, err := strconv.Unquote(rest[:end+1])
		if err != nil {
			return nil, err
		}
		args = append(args, value)
		rest = rest[end+1:]
	}
	return args, nil
}
