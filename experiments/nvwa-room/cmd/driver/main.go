package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/relaishq/relais/experiments/nvwa-room/client"
	"github.com/relaishq/relais/experiments/nvwa-room/harness"
	"github.com/relaishq/relais/experiments/nvwa-room/media"
	"github.com/relaishq/relais/experiments/nvwa-room/room"
)

type MessageResult struct {
	Index int        `json:"index"`
	Type  string     `json:"type"`
	Bytes int        `json:"bytes"`
	RTTMS float64    `json:"rtt_ms"`
	Error string     `json:"error,omitempty"`
	Reply room.Reply `json:"reply"`
}
type PlayerReport struct {
	ID               string               `json:"player_id"`
	Messages         []MessageResult      `json:"messages"`
	Observations     []client.Observation `json:"observations"`
	Audio            []media.AudioReport  `json:"audio_sessions"`
	RTTMeanMS        float64              `json:"rtt_mean_ms"`
	RTTMaxMS         float64              `json:"rtt_max_ms"`
	StateAgeMaxMS    float64              `json:"latest_state_age_max_ms"`
	Missing          int                  `json:"missing"`
	Duplicate        int                  `json:"duplicate_receipts"`
	Reordered        int                  `json:"reordered_receipts"`
	Reconnects       int                  `json:"reconnects"`
	RecoveryMS       float64              `json:"recovery_ms"`
	CutToRecoveredMS float64              `json:"cut_to_recovered_ms"`
	AudioMaxGapMS    float64              `json:"audio_max_gap_ms"`
	Version          uint64               `json:"final_room_version"`
	Errors           []string             `json:"errors"`
}
type InFlightCut struct {
	CommandID        string       `json:"command_id"`
	SentNS           int64        `json:"sent_ns"`
	AppliedBarrierNS int64        `json:"applied_barrier_ns"`
	CutNS            int64        `json:"cut_ns"`
	PendingAtCut     bool         `json:"pending_at_cut"`
	OriginalError    string       `json:"original_error"`
	OldGeneration    uint64       `json:"old_generation"`
	NewGeneration    uint64       `json:"new_generation"`
	Query            *room.Result `json:"query"`
	Retry            *room.Result `json:"retry"`
	Deduplicated     uint64       `json:"deduplicated"`
	SideEffects      uint64       `json:"side_effects"`
}
type Run struct {
	InFlight   *InFlightCut    `json:"in_flight_cut,omitempty"`
	Mode       string          `json:"mode"`
	Transport  string          `json:"transport"`
	Started    string          `json:"started"`
	Count      int             `json:"messages_per_client"`
	Rate       int             `json:"rate_hz"`
	FaultIndex int             `json:"fault_index"`
	KillPID    int             `json:"kill_pid,omitempty"`
	KillAt     string          `json:"kill_at,omitempty"`
	Players    []PlayerReport  `json:"players"`
	Metrics    room.Metrics    `json:"room_metrics"`
	Snapshot   room.Reply      `json:"snapshot"`
	Status     json.RawMessage `json:"control_status"`
	Errors     []string        `json:"errors"`
	DurationMS float64         `json:"duration_ms"`
}

func fetch(url string, dest any) error {
	h := http.Client{Timeout: time.Second}
	res, e := h.Get(url)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	return json.NewDecoder(res.Body).Decode(dest)
}
func main() {
	bin := flag.String("bin", "", "binary directory")
	out := flag.String("out", "", "private output directory")
	toneA := flag.String("tone-a", "", "440Hz Ogg")
	toneB := flag.String("tone-b", "", "880Hz Ogg")
	fault := flag.String("fault", "normal", "normal, disconnect, worker-kill, lost-receipt")
	count := flag.Int("count", 1000, "numbered app messages per client")
	rate := flag.Int("rate", 30, "messages per second per client")
	data := flag.Bool("datachannel", false, "stage 2 only, requires W1")
	flag.Parse()
	if *out == "" || *bin == "" || *rate < 1 || *rate > 50 || *count < 4 || *count > 2000 {
		log.Fatal("invalid bounded driver config")
	}
	switch *fault {
	case "normal", "disconnect", "worker-kill", "lost-receipt":
	default:
		log.Fatal("unknown fault")
	}
	if err := run(*bin, *out, *toneA, *toneB, *fault, *count, *rate, *data); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run(bin, out, toneA, toneB, fault string, count, rate int, data bool) error {
	root, _ := os.Getwd()
	bin, _ = filepath.Abs(bin)
	out, _ = filepath.Abs(out)
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	startup, stop := context.WithTimeout(ctx, 15*time.Second)
	top, err := harness.Start(startup, bin, root, out, data)
	stop()
	if err != nil {
		return err
	}
	defer top.Close()
	samplesA, err := media.LoadOpus(toneA)
	if err != nil {
		return err
	}
	samplesB, err := media.LoadOpus(toneB)
	if err != nil {
		return err
	}
	samples := [][][]byte{samplesA, samplesB}
	signal := &media.Signal{URL: top.Config.SignalURL, CertFile: top.Config.Cert, TokenFile: top.Config.Token}
	if err = signal.Init(); err != nil {
		return err
	}
	creds := make([]client.Credentials, 2)
	for i := range creds {
		creds[i], err = client.Lobby(ctx, top.Config.RoomURL)
		if err != nil {
			return err
		}
	}
	run := Run{Mode: fault, Transport: "websocket", Started: start.UTC().Format(time.RFC3339Nano), Count: count, Rate: rate, FaultIndex: ((count / 4) / 2) * 2, Players: make([]PlayerReport, 2)}
	if data {
		run.Transport = "relais-datachannel"
	}
	var calls [2]*media.Call
	var clients [2]*client.Client
	connect := func(i int, worker string) error {
		call, e := media.Dial(ctx, signal, worker, samples[i], samples[1-i], data)
		if e != nil {
			return e
		}
		calls[i] = call
		var conn client.MessageConn
		if data {
			conn = call.DataConn()
		} else {
			conn, e = client.DialWS(client.WSURL(top.Config.RoomURL))
			if e != nil {
				call.Close()
				return e
			}
		}
		clients[i], e = client.Connect(ctx, conn, creds[i], call.ID)
		if e != nil {
			call.Close()
		}
		return e
	}
	for i := range 2 {
		if err = connect(i, fmt.Sprintf("w%d", i+1)); err != nil {
			return err
		}
	}
	defer func() {
		for i := range 2 {
			if clients[i] != nil {
				_ = clients[i].Close()
			}
			if calls[i] != nil {
				calls[i].Close()
			}
		}
	}()
	// Require genuine peer media before the numbered app run.
	deadline := time.Now().Add(5 * time.Second)
	for calls[0].Report().PeerPayload < 10 || calls[1].Report().PeerPayload < 10 {
		if time.Now().After(deadline) {
			return errors.New("two-way peer audio did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := &run.Players[i]
			p.ID = creds[i].PlayerID
			next := time.Now()
			faultDone := false
			for n := 0; n < count; n++ {
				next = next.Add(time.Second / time.Duration(rate))
				if delay := time.Until(next); delay > 0 {
					timer := time.NewTimer(delay)
					select {
					case <-timer.C:
					case <-ctx.Done():
						timer.Stop()
						p.Errors = append(p.Errors, ctx.Err().Error())
						return
					}
				}
				payload := json.RawMessage(`{"x":7,"y":11,"pad":"0123456789abcdef0123456789abcdef"}`)
				m := room.Message{Type: "state", Seq: uint64(n + 1), SentNS: time.Now().UnixNano(), Payload: payload}
				if n%2 == 0 {
					m.Type = "trigger"
					m.CommandID = fmt.Sprintf("%s-%04d", p.ID, n)
					m.InputSeq = uint64(n + 1)
					m.Payload = json.RawMessage(`{"id":"wave","version":1,"start_tick":0}`)
					if n%4 == 2 {
						m.Type = "stop"
						m.Payload = json.RawMessage(`{}`)
					}
					m.Digest = room.Digest(m.Payload)
				}
				drop := i == 0 && fault == "lost-receipt" && n == ((count/4)/2)*2
				inFlight := i == 0 && n == ((count/4)/2)*2 && !faultDone && (fault == "disconnect" || fault == "worker-kill")
				if inFlight {
					m.TestPauseMS = 2000
				}
				m.DropReceipt = drop
				wire := m
				wire.RequestID = "r0000"
				wire.RoomID = creds[i].RoomID
				wire.Epoch = creds[i].Epoch
				wire.Generation = clients[i].Generation
				b, _ := json.Marshal(wire)
				before := time.Now()
				timeout := time.Second
				if drop {
					timeout = 150 * time.Millisecond
				}
				request, cancel := context.WithTimeout(ctx, timeout)
				var v room.Reply
				var e error
				if inFlight {
					faultDone = true
					cancel()
					proof := &InFlightCut{CommandID: m.CommandID, OldGeneration: clients[i].Generation}
					run.InFlight = proof
					sent := make(chan int64, 1)
					pending := make(chan error, 1)
					request, cancel = context.WithTimeout(ctx, 4*time.Second)
					go func() {
						_, err := clients[i].RequestWithSent(request, m, func() { sent <- time.Now().UnixNano() })
						pending <- err
					}()
					select {
					case proof.SentNS = <-sent:
					case <-request.Done():
						cancel()
						p.Errors = append(p.Errors, "in-flight command was not sent")
						return
					}
					deadline := time.Now().Add(time.Second)
					for {
						var metrics room.Metrics
						if err := fetch(top.Config.RoomURL+"/metrics", &metrics); err != nil {
							cancel()
							p.Errors = append(p.Errors, err.Error())
							return
						}
						if metrics.HeldCommandID == m.CommandID && metrics.HeldCommandApplications == 1 {
							proof.AppliedBarrierNS = time.Now().UnixNano()
							break
						}
						if time.Now().After(deadline) {
							cancel()
							p.Errors = append(p.Errors, "room did not reach applied/held-receipt barrier")
							return
						}
						time.Sleep(time.Millisecond)
					}
					select {
					case <-pending:
						cancel()
						p.Errors = append(p.Errors, "receipt arrived before cut")
						return
					default:
						proof.PendingAtCut = true
					}
					obs := clients[i].Observations()
					last := time.Now()
					if len(obs) > 0 {
						last = time.Unix(0, obs[len(obs)-1].AtNS)
					}
					cut := time.Now()
					proof.CutNS = cut.UnixNano()
					if fault == "worker-kill" {
						pid, at, err := top.KillWorker("worker-1")
						run.KillPID, run.KillAt = pid, at.UTC().Format(time.RFC3339Nano)
						if err != nil {
							cancel()
							p.Errors = append(p.Errors, err.Error())
							return
						}
					}
					p.Observations = append(p.Observations, obs...)
					_ = clients[i].Close()
					original := <-pending
					cancel()
					if original == nil {
						p.Errors = append(p.Errors, "cut request unexpectedly succeeded")
						return
					}
					proof.OriginalError = original.Error()
					calls[i].Close()
					p.Audio = append(p.Audio, calls[i].Report())
					if fault == "disconnect" {
						time.Sleep(250 * time.Millisecond)
					}
					if err := connect(i, "w2"); err != nil {
						p.Errors = append(p.Errors, err.Error())
						return
					}
					proof.NewGeneration = clients[i].Generation
					p.Reconnects++
					p.RecoveryMS = float64(time.Since(last)) / 1e6
					p.CutToRecoveredMS = float64(time.Since(cut)) / 1e6
					next = time.Now()
					request, cancel = context.WithTimeout(ctx, time.Second)
					q, qe := clients[i].Request(request, room.Message{Type: "query", CommandID: m.CommandID})
					cancel()
					proof.Query = q.Result
					m.TestPauseMS = 0
					request, cancel = context.WithTimeout(ctx, time.Second)
					v, e = clients[i].Request(request, m)
					cancel()
					proof.Retry = v.Result
					if qe != nil || q.Error != "" || q.Result == nil || !q.Result.Applied || v.Result == nil || *q.Result != *v.Result || proof.NewGeneration <= proof.OldGeneration {
						p.Errors = append(p.Errors, "in-flight query/retry/generation assertion failed")
					}
				} else {
					v, e = clients[i].Request(request, m)
					cancel()
				}
				if drop {
					if !errors.Is(e, context.DeadlineExceeded) {
						p.Errors = append(p.Errors, "receipt fault did not time out")
					}
					request, cancel = context.WithTimeout(ctx, time.Second)
					q, qe := clients[i].Request(request, room.Message{Type: "query", CommandID: m.CommandID})
					cancel()
					if qe != nil || q.Result == nil || !q.Result.Applied {
						p.Errors = append(p.Errors, fmt.Sprintf("receipt query failed: %v %+v", qe, q))
					}
					m.DropReceipt = false
					request, cancel = context.WithTimeout(ctx, time.Second)
					v, e = clients[i].Request(request, m)
					cancel()
					if q.Result == nil || v.Result == nil || *q.Result != *v.Result {
						p.Errors = append(p.Errors, "retry changed recorded result")
					}
					conflict := m
					conflict.InputSeq++
					request, cancel = context.WithTimeout(ctx, time.Second)
					bad, be := clients[i].Request(request, conflict)
					cancel()
					if be != nil || bad.Error != "command_conflict" {
						p.Errors = append(p.Errors, "conflict was not explicit")
					}
				}
				metric := MessageResult{Index: n, Type: m.Type, Bytes: len(b), RTTMS: float64(time.Since(before)) / 1e6, Reply: v}
				if e != nil {
					metric.Error = e.Error()
					p.Missing++
				} else if v.Error != "" {
					metric.Error = v.Error
					p.Missing++
				}
				p.Messages = append(p.Messages, metric)
				if metric.Error != "" {
					p.Errors = append(p.Errors, metric.Error)
				}
				if metric.RTTMS > p.RTTMaxMS {
					p.RTTMaxMS = metric.RTTMS
				}
				p.RTTMeanMS += metric.RTTMS
			}
			p.RTTMeanMS /= float64(count)
		}(i)
	}
	wg.Wait()
	for i := range 2 {
		p := &run.Players[i]
		v, e := clients[i].Request(ctx, room.Message{Type: "snapshot"})
		if e != nil {
			p.Errors = append(p.Errors, e.Error())
		}
		p.Version = v.Version
		p.Observations = append(p.Observations, clients[i].Observations()...)
		p.Audio = append(p.Audio, calls[i].Report())
		_ = clients[i].Close()
		calls[i].Close()
		seen := map[uint64]bool{}
		last := uint64(0)
		for _, m := range p.Messages {
			if m.Type == "state" && m.Error == "" {
				if seen[m.Reply.Seq] {
					p.Duplicate++
				}
				seen[m.Reply.Seq] = true
				if m.Reply.Seq < last {
					p.Reordered++
				}
				last = m.Reply.Seq
			}
		}
		for _, o := range p.Observations {
			for _, state := range o.Reply.Players {
				if state.StateSentNS > 0 {
					age := float64(o.AtNS-state.StateSentNS) / 1e6
					if age > p.StateAgeMaxMS {
						p.StateAgeMaxMS = age
					}
				}
			}
		}
		var arrivals []int64
		for _, a := range p.Audio {
			arrivals = append(arrivals, a.ArrivalsNS...)
			if a.SelfPayload != 0 || a.UnknownPayload != 0 || a.PeerPayload < 10 {
				p.Errors = append(p.Errors, fmt.Sprintf("audio evidence failure: peer=%d self=%d unknown=%d", a.PeerPayload, a.SelfPayload, a.UnknownPayload))
			}
		}
		sort.Slice(arrivals, func(a, b int) bool { return arrivals[a] < arrivals[b] })
		for n := 1; n < len(arrivals); n++ {
			gap := float64(arrivals[n]-arrivals[n-1]) / 1e6
			if gap > p.AudioMaxGapMS {
				p.AudioMaxGapMS = gap
			}
		}
	}
	time.Sleep(100 * time.Millisecond)
	_ = fetch(top.Config.RoomURL+"/metrics", &run.Metrics)
	_ = fetch(top.Config.RoomURL+"/snapshot", &run.Snapshot)
	if run.InFlight != nil {
		run.InFlight.Deduplicated = run.Metrics.Deduplicated
		run.InFlight.SideEffects = run.Metrics.HeldCommandApplications
		if run.InFlight.Deduplicated != 1 || run.InFlight.SideEffects != 1 {
			run.Errors = append(run.Errors, "in-flight dedup/side-effect count must each be exactly one")
		}
	}
	run.Status, _ = top.Status(ctx)
	run.DurationMS = float64(time.Since(start)) / 1e6
	if run.Metrics.Applied != uint64(2*((count+1)/2)) {
		run.Errors = append(run.Errors, fmt.Sprintf("side effects: applied=%d expected=%d", run.Metrics.Applied, 2*((count+1)/2)))
	}
	if run.Players[0].Version != run.Players[1].Version {
		run.Errors = append(run.Errors, "final snapshots diverged")
	}
	for _, p := range run.Players {
		run.Errors = append(run.Errors, p.Errors...)
	}
	b, _ := json.MarshalIndent(run, "", "  ")
	if err = os.WriteFile(filepath.Join(out, "result.json"), b, 0600); err != nil {
		return err
	}
	summary := map[string]any{"mode": fault, "transport": run.Transport, "duration_ms": run.DurationMS, "metrics": run.Metrics, "errors": run.Errors}
	for i, p := range run.Players {
		summary[fmt.Sprintf("p%d", i+1)] = map[string]any{"messages": len(p.Messages), "rtt_mean_ms": p.RTTMeanMS, "rtt_max_ms": p.RTTMaxMS, "latest_state_age_max_ms": p.StateAgeMaxMS, "missing": p.Missing, "duplicate": p.Duplicate, "reorder": p.Reordered, "reconnects": p.Reconnects, "recovery_ms": p.RecoveryMS, "audio_max_gap_ms": p.AudioMaxGapMS, "version": p.Version, "audio": p.Audio}
	}
	// Keep stdout compact; raw packet arrivals and replies are in result.json.
	for _, p := range run.Players {
		for n := range p.Audio {
			p.Audio[n].ArrivalsNS = nil
		}
	}
	encoded, _ := json.Marshal(summary)
	fmt.Println(string(encoded))
	if len(run.Errors) > 0 {
		return fmt.Errorf("run has %d failed assertions; see result.json", len(run.Errors))
	}
	return nil
}
