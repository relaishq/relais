package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/relaishq/relais/experiments/nvwa-room/client"
	"github.com/relaishq/relais/experiments/nvwa-room/harness"
	"github.com/relaishq/relais/experiments/nvwa-room/media"
	"github.com/relaishq/relais/experiments/nvwa-room/room"
)

func main() {
	bin := flag.String("bin", "", "binary directory")
	out := flag.String("out", "", "output directory")
	a := flag.String("tone-a", "", "440Hz Ogg")
	b := flag.String("tone-b", "", "880Hz Ogg")
	flag.Parse()
	if e := run(*bin, *out, *a, *b); e != nil {
		log.Fatal(e)
	}
}
func run(bin, out, a, b string) error {
	root, _ := os.Getwd()
	bin, _ = filepath.Abs(bin)
	out, _ = filepath.Abs(out)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	top, e := harness.Start(ctx, bin, root, out, true)
	if e != nil {
		return e
	}
	defer top.Close()
	signal := &media.Signal{URL: top.Config.SignalURL, CertFile: top.Config.Cert, TokenFile: top.Config.Token}
	if e = signal.Init(); e != nil {
		return e
	}
	ta, e := media.LoadOpus(a)
	if e != nil {
		return e
	}
	tb, e := media.LoadOpus(b)
	if e != nil {
		return e
	}
	tones := [][][]byte{ta, tb}
	var creds [2]client.Credentials
	var calls [2]*media.Call
	var consumers [2]*client.Client
	defer func() {
		for i := range 2 {
			if consumers[i] != nil {
				_ = consumers[i].Close()
			}
			if calls[i] != nil {
				calls[i].Close()
			}
		}
	}()
	connect := func(i int, worker string) error {
		var err error
		calls[i], err = media.Dial(ctx, signal, worker, tones[i], tones[1-i], true)
		if err != nil {
			return err
		}
		consumers[i], err = client.Connect(ctx, calls[i].DataConn(), creds[i], calls[i].ID)
		return err
	}
	for i := range 2 {
		creds[i], e = client.Lobby(ctx, top.Config.RoomURL)
		if e != nil {
			return e
		}
		if e = connect(i, fmt.Sprintf("w%d", i+1)); e != nil {
			return e
		}
	}
	waitAudio := func(i int) error {
		deadline := time.Now().Add(5 * time.Second)
		for calls[i].Report().PeerPayload < 20 {
			if time.Now().After(deadline) {
				return fmt.Errorf("peer audio timeout")
			}
			time.Sleep(20 * time.Millisecond)
		}
		return nil
	}
	for i := range 2 {
		if e = waitAudio(i); e != nil {
			return e
		}
	}
	payload := json.RawMessage(`{"id":"wave","version":1,"start_tick":0}`)
	command := room.Message{Type: "trigger", CommandID: "policy-stable-command", InputSeq: 1, Payload: payload, Digest: room.Digest(payload)}
	initial, e := consumers[0].Request(ctx, command)
	if e != nil || initial.Result == nil {
		return fmt.Errorf("initial command: %v %+v", e, initial)
	}
	post := func(path string) (int, json.RawMessage, error) {
		request, err := http.NewRequestWithContext(ctx, "POST", top.Config.Control+path, nil)
		if err != nil {
			return 0, nil, err
		}
		res, err := http.DefaultClient.Do(request)
		if err != nil {
			return 0, nil, err
		}
		defer res.Body.Close()
		data, err := io.ReadAll(io.LimitReader(res.Body, 65536))
		return res.StatusCode, data, err
	}
	before := calls[0].Report()
	moveStatus, moveBody, e := post("/calls/" + calls[0].ID + "/move?to=w2")
	if e != nil {
		return e
	}
	if moveStatus < 400 || !strings.Contains(string(moveBody), "data_session_not_movable") {
		return fmt.Errorf("move not definitively refused: %d %s", moveStatus, moveBody)
	}
	afterMove, e := consumers[0].Request(ctx, room.Message{Type: "snapshot"})
	if e != nil {
		return fmt.Errorf("move affected app: %w", e)
	}
	time.Sleep(300 * time.Millisecond)
	moveAudio := calls[0].Report()
	if moveAudio.PeerPayload <= before.PeerPayload {
		return fmt.Errorf("move affected audio")
	}
	beforeDrainPeer := calls[1].Report().PeerPayload
	oldID := calls[0].ID
	drainAt := time.Now()
	drainStatus, drainBody, e := post("/workers/w1/drain")
	if e != nil {
		return e
	}
	if drainStatus < 200 || drainStatus >= 300 || !strings.Contains(string(drainBody), `"ended"`) {
		return fmt.Errorf("drain not reconnect-required: %d %s", drainStatus, drainBody)
	}
	var metrics room.Metrics
	deadline := time.Now().Add(2 * time.Second)
	gotReason := false
	for !gotReason && time.Now().Before(deadline) {
		res, err := http.Get(top.Config.RoomURL + "/metrics")
		if err != nil {
			return err
		}
		err = json.NewDecoder(res.Body).Decode(&metrics)
		res.Body.Close()
		if err != nil {
			return err
		}
		for _, event := range metrics.Events {
			if event.Op == 3 && event.SessionID == oldID && event.Reason == "reconnect_required" {
				gotReason = true
			}
		}
		if !gotReason {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !gotReason {
		return fmt.Errorf("room did not observe op3 reconnect_required")
	}
	timeout, stop := context.WithTimeout(ctx, 150*time.Millisecond)
	_, oldErr := consumers[0].Request(timeout, room.Message{Type: "snapshot"})
	stop()
	if oldErr == nil {
		return fmt.Errorf("drained app session still works")
	}
	oldAudio := calls[0].Report()
	oldObservations := consumers[0].Observations()
	_ = consumers[0].Close()
	calls[0].Close()
	if e = connect(0, "w2"); e != nil {
		return e
	}
	recovery := time.Since(drainAt)
	for i := range 2 {
		if e = waitAudio(i); e != nil {
			return e
		}
	}
	recovered, e := consumers[0].Request(ctx, room.Message{Type: "snapshot"})
	if e != nil {
		return e
	}
	queried, e := consumers[0].Request(ctx, room.Message{Type: "query", CommandID: command.CommandID})
	if e != nil || queried.Result == nil || *queried.Result != *initial.Result {
		return fmt.Errorf("command result not preserved after drain")
	}
	retry, e := consumers[0].Request(ctx, command)
	if e != nil || retry.Result == nil || *retry.Result != *initial.Result {
		return fmt.Errorf("drain retry changed result")
	}
	if calls[0].ID == oldID || consumers[0].Generation != 2 {
		return fmt.Errorf("drain did not replace session/generation")
	}
	if calls[1].Report().PeerPayload < beforeDrainPeer+10 {
		return fmt.Errorf("peer did not receive resumed caller audio")
	}
	finalMetrics, err := http.Get(top.Config.RoomURL + "/metrics")
	if err != nil {
		return err
	}
	err = json.NewDecoder(finalMetrics.Body).Decode(&metrics)
	finalMetrics.Body.Close()
	if err != nil {
		return err
	}
	if metrics.Applied != 1 || metrics.Deduplicated != 1 {
		return fmt.Errorf("drain retry duplicated side effect: %+v", metrics)
	}
	audio := []media.AudioReport{oldAudio, calls[0].Report(), calls[1].Report()}
	gap := 0.0
	if len(oldAudio.ArrivalsNS) > 0 && len(audio[1].ArrivalsNS) > 0 {
		gap = float64(audio[1].ArrivalsNS[0]-oldAudio.ArrivalsNS[len(oldAudio.ArrivalsNS)-1]) / 1e6
	}
	for _, r := range audio {
		if r.SelfPayload != 0 || r.UnknownPayload != 0 || r.PeerPayload < 20 {
			return fmt.Errorf("invalid peer audio after policy")
		}
	}
	result := map[string]any{"move_http_status": moveStatus, "move_response": moveBody, "move_snapshot": afterMove, "move_audio_before": before.PeerPayload, "move_audio_after": moveAudio.PeerPayload, "peer_audio_before_drain": beforeDrainPeer, "peer_audio_after_reconnect": calls[1].Report().PeerPayload, "drain_http_status": drainStatus, "drain_response": drainBody, "observed_upstream_reconnect_required": gotReason, "old_client_error": fmt.Sprint(oldErr), "old_session_id": oldID, "new_session_id": calls[0].ID, "new_generation": consumers[0].Generation, "drain_recovery_ms": float64(recovery) / 1e6, "audio_cross_session_gap_ms": gap, "audio": audio, "snapshot": recovered, "command_result": queried, "retry": retry, "room_metrics": metrics, "old_observations": oldObservations, "new_observations": consumers[0].Observations()}
	encoded, _ := json.MarshalIndent(result, "", "  ")
	if e = os.WriteFile(filepath.Join(out, "result.json"), encoded, 0600); e != nil {
		return e
	}
	fmt.Printf("move_status=%d move_unaffected=true drain_status=%d op3_reason=reconnect_required new_generation=%d recovery_ms=%.3f audio_gap_ms=%.3f\n", moveStatus, drainStatus, consumers[0].Generation, float64(recovery)/1e6, gap)
	return nil
}
