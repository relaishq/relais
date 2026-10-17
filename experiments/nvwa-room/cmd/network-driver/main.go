// network-driver is a disposable fixed-cadence comparison, not a product client.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/relaishq/relais/experiments/nvwa-room/client"
	"github.com/relaishq/relais/experiments/nvwa-room/harness"
	"github.com/relaishq/relais/experiments/nvwa-room/media"
	"github.com/relaishq/relais/experiments/nvwa-room/room"
	"github.com/relaishq/relais/internal/nettopology"
	"github.com/relaishq/relais/pkg/controlplane"
)

type options struct {
	Bin, Out, ToneA, ToneB, Topology, Profile, Variant, Fault string
	Count, Rate                                               int
	Config                                                    harness.Config
	Socket                                                    string
}
type sample struct {
	Index         int        `json:"index"`
	Type          string     `json:"type"`
	SentNS        int64      `json:"sent_ns"`
	Bytes         int        `json:"bytes"`
	ScheduleLagMS float64    `json:"schedule_lag_ms"`
	RTTMS         float64    `json:"rtt_ms,omitempty"`
	Error         string     `json:"error,omitempty"`
	Reply         room.Reply `json:"reply,omitempty"`
}
type playerReport struct {
	ID           string               `json:"id"`
	Messages     []sample             `json:"messages"`
	Observations []client.Observation `json:"observations"`
	Audio        []media.AudioReport  `json:"audio"`
}
type proof struct {
	CommandID                             string `json:"command_id"`
	OldSession                            string `json:"old_session"`
	SentNS, BarrierNS, CutNS, RecoveredNS int64
	RecoveryMS                            *float64 `json:"recovery_ms"`
	PendingAtCut                          bool     `json:"pending_at_cut"`
	OriginalError                         string   `json:"original_error"`
	OldGeneration, NewGeneration          uint64
	Query, Retry                          *room.Result
	Takeover                              *controlplane.MoveResult `json:"takeover,omitempty"`
	Deduplicated, SideEffects             uint64
}
type report struct {
	Profile        string `json:"profile"`
	Topology       string `json:"topology"`
	Variant        string `json:"variant"`
	Fault          string `json:"fault"`
	Count          int    `json:"messages_per_client"`
	Rate           int    `json:"rate_hz"`
	StartNS, EndNS int64
	Players        [2]playerReport `json:"players"`
	Proof          proof           `json:"proof"`
	Metrics        room.Metrics    `json:"room_metrics"`
	Errors         []string        `json:"errors"`
	DurationMS     float64         `json:"duration_ms"`
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0600)
}
func getJSON(ctx context.Context, h *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	res, err := h.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("GET %s: %s", url, res.Status)
	}
	return json.NewDecoder(res.Body).Decode(v)
}
func main() {
	var o options
	flag.StringVar(&o.Bin, "bin", "", "spike binary directory")
	flag.StringVar(&o.Out, "out", "", "fresh evidence directory")
	flag.StringVar(&o.ToneA, "tone-a", "", "440Hz Opus fixture")
	flag.StringVar(&o.ToneB, "tone-b", "", "880Hz Opus fixture")
	flag.StringVar(&o.Topology, "topology", "loopback", "loopback or netns")
	flag.StringVar(&o.Profile, "profile", "lan", "lan, region, far-caller, lossy-dc, poor-wifi")
	flag.StringVar(&o.Variant, "variant", "ws", "ws, dc-reliable, dc-unreliable")
	flag.StringVar(&o.Fault, "fault", "normal", "normal, disconnect, worker-kill, lost-receipt")
	flag.IntVar(&o.Count, "count", 1000, "messages per client, alternating command/state")
	flag.IntVar(&o.Rate, "rate", 30, "total messages/sec/client")
	cleanupRoot := flag.String("cleanup-root", "", "reclaim only stale owners recorded below this profile output root")
	callerConfig := flag.String("caller-config", "", "internal re-exec configuration")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var err error
	if *cleanupRoot != "" {
		err = reclaimRecorded(ctx, *cleanupRoot)
	} else if *callerConfig != "" {
		var b []byte
		b, err = os.ReadFile(*callerConfig)
		if err == nil {
			err = json.Unmarshal(b, &o)
		}
		if err == nil {
			err = runCaller(ctx, o)
		}
	} else {
		err = validate(o)
		if err == nil {
			err = runOwner(ctx, o)
		}
	}
	if err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func validate(o options) error {
	if o.Out == "" || o.Bin == "" || o.Count < 4 || o.Count > 2000 || o.Rate < 1 || o.Rate > 50 {
		return errors.New("invalid bounded config")
	}
	if o.Variant != "ws" && o.Variant != "dc-reliable" && o.Variant != "dc-unreliable" {
		return errors.New("invalid variant")
	}
	if o.Fault != "normal" && o.Fault != "disconnect" && o.Fault != "worker-kill" && o.Fault != "lost-receipt" {
		return errors.New("invalid fault")
	}
	if o.Topology != "netns" && o.Topology != "loopback" {
		return errors.New("invalid topology")
	}
	if o.Profile != "poor-wifi" {
		if _, err := nettopology.ParseProfile(o.Profile); err != nil {
			return err
		}
	}
	if o.Topology == "loopback" && o.Profile != "lan" {
		return errors.New("loopback cannot claim an impaired profile")
	}
	return nil
}
func runOwner(ctx context.Context, o options) (retErr error) {
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	o.Bin, err = filepath.Abs(o.Bin)
	if err != nil {
		return err
	}
	o.Out, err = filepath.Abs(o.Out)
	if err != nil {
		return err
	}
	if err = os.Mkdir(o.Out, 0700); err != nil {
		return err
	}
	phase := "startup"
	defer func() {
		state := map[string]any{"phase": phase, "ok": retErr == nil}
		if retErr != nil {
			state["error"] = retErr.Error()
		}
		retErr = errors.Join(retErr, writeJSON(filepath.Join(o.Out, "cell-status.json"), state))
	}()
	if err = writeJSON(filepath.Join(o.Out, "cell-status.json"), map[string]any{"phase": phase, "ok": false}); err != nil {
		return err
	}
	startup, cancelStartup := context.WithTimeout(ctx, 60*time.Second)
	defer cancelStartup()
	var n *nettopology.Topology
	if o.Topology == "netns" {
		if err = writeJSON(filepath.Join(o.Out, "namespace-owner.json"), nettopology.OwnerRecord{UID: os.Getuid(), PID: os.Getpid()}); err != nil {
			return err
		}
		n, err = nettopology.Open(startup, log.Printf)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, n.Close()) }()
	}
	top, err := harness.StartNetwork(startup, o.Bin, root, o.Out, o.Variant != "ws", n)
	if err != nil {
		return err
	}
	defer top.Close()
	o.Config = top.Config
	if n != nil {
		if err = harness.ConfigureNetwork(startup, n, top.Config, o.Profile, o.Out); err != nil {
			return err
		}
	}
	cancelStartup()
	// A Unix control socket keeps kill/metrics traffic off both measured paths.
	// Use a short temporary path: macOS Unix sockets cap paths at 104 bytes.
	socketDir, err := os.MkdirTemp("", "nvwa-net-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(socketDir)
	o.Socket = filepath.Join(socketDir, "control.sock")
	listener, err := net.Listen("unix", o.Socket)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /kill", func(w http.ResponseWriter, _ *http.Request) {
		pid, at, e := top.KillWorker("worker-1")
		if e != nil {
			http.Error(w, e.Error(), 500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"pid": pid, "at_ns": at.UnixNano()})
	})
	for _, path := range []string{"/metrics", "/snapshot", "/status"} {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, req *http.Request) {
			url := top.Config.RoomURL + path
			if path == "/status" {
				url = top.Config.Control + path
			}
			var value json.RawMessage
			if e := getJSON(req.Context(), &http.Client{Timeout: 5 * time.Second}, url, &value); e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
			_ = json.NewEncoder(w).Encode(value)
		})
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	config := filepath.Join(o.Out, "caller-config.json")
	if err = writeJSON(config, o); err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"-caller-config", config}
	cmd := exec.CommandContext(ctx, binary, args...)
	if n != nil {
		cmd = exec.CommandContext(ctx, "ip", append([]string{"netns", "exec", n.Plan.Roles["caller"].Namespace, binary}, args...)...)
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	phase = "workload"
	if err = writeJSON(filepath.Join(o.Out, "cell-status.json"), map[string]any{"phase": phase, "ok": false}); err != nil {
		return err
	}
	return cmd.Run()
}
func runCaller(ctx context.Context, o options) (retErr error) {
	started := time.Now()
	r := report{Profile: o.Profile, Topology: o.Topology, Variant: o.Variant, Fault: o.Fault, Count: o.Count, Rate: o.Rate}
	defer func() {
		if retErr != nil {
			r.Errors = append(r.Errors, retErr.Error())
		}
		r.DurationMS = float64(time.Since(started)) / 1e6
		retErr = errors.Join(retErr, writeJSON(filepath.Join(o.Out, "result.json"), r))
	}()
	control := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", o.Socket)
	}}}
	defer control.CloseIdleConnections()
	signal := &media.Signal{URL: o.Config.SignalURL, CertFile: o.Config.Cert, TokenFile: o.Config.Token}
	if err := signal.Init(); err != nil {
		return err
	}
	a, err := media.LoadOpus(o.ToneA)
	if err != nil {
		return err
	}
	b, err := media.LoadOpus(o.ToneB)
	if err != nil {
		return err
	}
	tones := [][][]byte{a, b}
	var creds [2]client.Credentials
	var calls [2]*media.Call
	var clients [2]*client.Client
	connect := func(i int, worker string) error {
		var e error
		calls[i], e = media.DialPolicy(ctx, signal, worker, tones[i], tones[1-i], o.Variant != "ws", o.Variant == "dc-reliable")
		if e != nil {
			return e
		}
		var conn client.MessageConn
		if o.Variant == "ws" {
			conn, e = client.DialWS(client.WSURL(o.Config.RoomURL))
		} else {
			conn = calls[i].DataConn()
		}
		if e != nil {
			return e
		}
		join, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		clients[i], e = client.Connect(join, conn, creds[i], calls[i].ID)

		return e
	}
	defer func() {
		for i := range 2 {
			if clients[i] != nil {
				r.Players[i].Observations = append(r.Players[i].Observations, clients[i].Observations()...)
				_ = clients[i].Close()
			}
			if calls[i] != nil {
				r.Players[i].Audio = append(r.Players[i].Audio, calls[i].Report())
				calls[i].Close()
			}
		}
	}()
	for i := range 2 {
		creds[i], err = client.Lobby(ctx, o.Config.RoomURL)
		if err != nil {
			return err
		}
		r.Players[i].ID = creds[i].PlayerID
		if err = connect(i, fmt.Sprintf("w%d", i+1)); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for calls[0].Report().PeerPayload < 10 || calls[1].Report().PeerPayload < 10 {
		if time.Now().After(deadline) {
			return errors.New("two-way peer audio did not start")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	// The same two producers run at a fixed cadence, independent of receipts.
	r.StartNS = time.Now().Add(100 * time.Millisecond).UnixNano()
	var producers sync.WaitGroup
	for i := range 2 {
		producers.Add(1)
		go func(i int) {
			defer producers.Done()
			p := &r.Players[i]
			p.Messages = make([]sample, o.Count)
			var pending sync.WaitGroup
			for index := range o.Count {
				scheduled := time.Unix(0, r.StartNS).Add(time.Duration(index) * time.Second / time.Duration(o.Rate))
				if d := time.Until(scheduled); d > 0 {
					timer := time.NewTimer(d)
					select {
					case <-timer.C:
					case <-ctx.Done():
						timer.Stop()
						return
					}
				}
				m := room.Message{Type: "state", Seq: uint64(index + 1), SentNS: time.Now().UnixNano(), Payload: json.RawMessage(`{"x":7,"y":11,"pad":"0123456789abcdef0123456789abcdef"}`)}
				if index%2 == 0 {
					m.Type, m.CommandID, m.InputSeq = "trigger", fmt.Sprintf("%s-%04d", p.ID, index), uint64(index+1)
					m.Payload = json.RawMessage(`{"id":"wave","version":1,"start_tick":0}`)
					if index%4 == 2 {
						m.Type, m.Payload = "stop", json.RawMessage(`{}`)
					}
					m.Digest = room.Digest(m.Payload)
				}
				s := &p.Messages[index]
				*s = sample{Index: index, Type: m.Type, SentNS: m.SentNS, ScheduleLagMS: float64(time.Since(scheduled)) / 1e6}
				if m.Type == "state" {
					size, e := clients[i].SendState(m)
					s.Bytes = size
					if e != nil {
						s.Error = e.Error()
					}
				} else {
					pending.Add(1)
					go func(m room.Message, s *sample) {
						defer pending.Done()
						request, cancel := context.WithTimeout(ctx, 10*time.Second)
						defer cancel()
						wire := m
						wire.RequestID = "r00000"
						wire.RoomID, wire.Epoch, wire.Generation = creds[i].RoomID, creds[i].Epoch, clients[i].Generation
						encoded, _ := json.Marshal(wire)
						s.Bytes = len(encoded)
						sent := time.Now()
						v, e := clients[i].RequestWithSent(request, m, func() { sent = time.Now(); s.SentNS = sent.UnixNano() })
						s.RTTMS, s.Reply = float64(time.Since(sent))/1e6, v
						if v.RequestID != "" {
							wire.RequestID = v.RequestID
							encoded, _ = json.Marshal(wire)
							s.Bytes = len(encoded)
						}
						if e != nil {
							s.Error = e.Error()
						} else if v.Error != "" {
							s.Error = v.Error
						}
					}(m, s)
				}
			}
			pending.Wait()
		}(i)
	}
	producers.Wait()
	// Retain delayed deliveries without extending the producer's sending window.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
	}
	r.EndNS = time.Now().UnixNano()
	for i := range 2 {
		for _, s := range r.Players[i].Messages {
			if s.Error != "" {
				r.Errors = append(r.Errors, fmt.Sprintf("p%d message %d: %s", i+1, s.Index, s.Error))
			}
		}
	}
	// Freeze the steady window before fault injection. Faults are separate metrics.
	if err = exerciseFault(ctx, o, control, &r, &clients, &calls, connect); err != nil {
		return err
	}
	if err = getJSON(ctx, control, "http://owned/metrics", &r.Metrics); err != nil {
		return err
	}
	r.Proof.Deduplicated, r.Proof.SideEffects = r.Metrics.Deduplicated, r.Metrics.HeldCommandApplications
	expected := uint64(2*((o.Count+1)/2) + 1)
	if r.Metrics.Applied != expected || r.Metrics.Deduplicated != 1 {
		r.Errors = append(r.Errors, fmt.Sprintf("side effects/dedup: got %d/%d expected %d/1", r.Metrics.Applied, r.Metrics.Deduplicated, expected))
	}
	if (o.Fault == "disconnect" || o.Fault == "worker-kill") && r.Proof.SideEffects != 1 {
		r.Errors = append(r.Errors, "held command side effect must be exactly one")
	}
	if len(r.Errors) > 0 {
		return fmt.Errorf("%d assertions failed", len(r.Errors))
	}
	return nil
}

func exerciseFault(ctx context.Context, o options, h *http.Client, r *report, clients *[2]*client.Client, calls *[2]*media.Call, connect func(int, string) error) error {
	// Arrays here hold the current handles; connect updates the owner's arrays.
	c, call := clients[0], calls[0]
	p := &r.Proof
	p.CommandID, p.OldGeneration, p.OldSession = "fault-command", c.Generation, call.ID
	m := room.Message{Type: "trigger", CommandID: p.CommandID, InputSeq: uint64(o.Count + 1), Payload: json.RawMessage(`{"id":"wave","version":1,"start_tick":0}`)}
	m.Digest = room.Digest(m.Payload)
	request := func(c *client.Client, m room.Message) (room.Reply, error) {
		bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return c.Request(bounded, m)
	}
	var original room.Reply
	var err error
	if o.Fault == "disconnect" || o.Fault == "worker-kill" {
		m.TestPauseMS = 2000
		m.TestBarrier = true
		if o.Fault == "worker-kill" && o.Variant == "ws" {
			m.TestPauseMS = 0
			m.DropReceipt = true
		}
		pending := make(chan error, 1)
		sent := make(chan struct{}, 1)
		originalCtx, cancelOriginal := context.WithTimeout(ctx, 20*time.Second)
		defer cancelOriginal()
		go func() {
			_, e := c.RequestWithSent(originalCtx, m, func() { p.SentNS = time.Now().UnixNano(); sent <- struct{}{} })
			pending <- e
		}()
		select {
		case <-sent:
		case <-ctx.Done():
			return ctx.Err()
		}
		deadline := time.Now().Add(1500 * time.Millisecond)
		for {
			var metrics room.Metrics
			if err = getJSON(ctx, h, "http://owned/metrics", &metrics); err != nil {
				return err
			}
			if metrics.HeldCommandID == m.CommandID && metrics.HeldCommandApplications == 1 {
				p.BarrierNS = time.Now().UnixNano()
				break
			}
			if time.Now().After(deadline) {
				return errors.New("held receipt barrier not reached before cut")
			}
			time.Sleep(5 * time.Millisecond)
		}
		select {
		case <-pending:
			return errors.New("receipt arrived before cut")
		default:
			p.PendingAtCut = true
		}
		p.CutNS = time.Now().UnixNano()
		if o.Fault == "worker-kill" {
			req, e := http.NewRequestWithContext(ctx, "POST", "http://owned/kill", nil)
			if e != nil {
				return e
			}
			res, e := h.Do(req)
			if e != nil {
				return e
			}
			var kill struct {
				AtNS int64 `json:"at_ns"`
			}
			e = json.NewDecoder(res.Body).Decode(&kill)
			_ = res.Body.Close()
			if e != nil {
				return e
			}
			p.CutNS = kill.AtNS
		}
		if o.Fault == "worker-kill" && o.Variant == "ws" {
			// WS survives worker death. Prove it using a real request on the same socket.
			v, e := request(c, room.Message{Type: "snapshot"})
			if e != nil || v.Error != "" {
				return fmt.Errorf("WS continuity: %v %s", e, v.Error)
			}
			p.RecoveredNS = time.Now().UnixNano()
			// The receipt was deliberately suppressed; cancel only its waiter.
			cancelOriginal()
			select {
			case err = <-pending:
			case <-ctx.Done():
				return ctx.Err()
			}
			if !errors.Is(err, context.Canceled) {
				return fmt.Errorf("suppressed receipt did not remain pending: %v", err)
			}
			p.OriginalError = err.Error()
		} else {
			if o.Fault == "worker-kill" {
				// DC is reconnect-only. Wait for the actual Relais takeover classification
				// before closing the old call and joining a new PC on the surviving worker.
				deadline = time.Now().Add(15 * time.Second)
				for {
					var status controlplane.Status
					if err = getJSON(ctx, h, "http://owned/status", &status); err != nil {
						return err
					}
					for _, event := range status.Takeovers {
						if event.ID == call.ID {
							copy := event
							p.Takeover = &copy
						}
					}
					if p.Takeover != nil {
						break
					}
					if time.Now().After(deadline) {
						return errors.New("relais takeover was not observed")
					}
					time.Sleep(20 * time.Millisecond)
				}
				if p.Takeover.LossCause != controlplane.LossCauseDataSessionReconnectRequired {
					return fmt.Errorf("unexpected DC takeover: %+v", p.Takeover)
				}
			}
			r.Players[0].Observations = append(r.Players[0].Observations, c.Observations()...)
			_ = c.Close()
			select {
			case err = <-pending:
			case <-ctx.Done():
				return ctx.Err()
			}
			if err == nil {
				return errors.New("cut request unexpectedly succeeded")
			}
			p.OriginalError = err.Error()
			r.Players[0].Audio = append(r.Players[0].Audio, call.Report())
			call.Close()
			if o.Fault == "disconnect" {
				time.Sleep(250 * time.Millisecond)
			}
			if err = connect(0, "w2"); err != nil {
				return err
			}
			// connect updates the owner arrays shared with this function.
			c = clients[0]
			p.NewGeneration = c.Generation
			if p.NewGeneration <= p.OldGeneration {
				return errors.New("reconnect generation did not advance")
			}
			if _, err = request(c, room.Message{Type: "snapshot"}); err != nil {
				return err
			}
			p.RecoveredNS = time.Now().UnixNano()
		}
		value := float64(p.RecoveredNS-p.CutNS) / 1e6
		p.RecoveryMS = &value
	} else {
		if o.Fault == "lost-receipt" {
			m.DropReceipt = true
		}
		bounded, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		if o.Fault == "normal" {
			cancel()
			bounded, cancel = context.WithTimeout(ctx, 10*time.Second)
		}
		original, err = c.Request(bounded, m)
		cancel()
		if o.Fault == "lost-receipt" {
			if !errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("expected lost receipt timeout: %v", err)
			}
		} else if err != nil || original.Error != "" {
			return fmt.Errorf("proof command: %v %s", err, original.Error)
		}
	}
	q, err := request(c, room.Message{Type: "query", CommandID: m.CommandID})
	if err != nil {
		return err
	}
	m.TestPauseMS, m.DropReceipt, m.TestBarrier = 0, false, false
	retry, err := request(c, m)
	if err != nil {
		return err
	}
	p.Query, p.Retry = q.Result, retry.Result
	if q.Error != "" || retry.Error != "" || q.Result == nil || retry.Result == nil || !q.Result.Applied || *q.Result != *retry.Result {
		return errors.New("query/retry changed exactly-once result")
	}
	return nil
}
