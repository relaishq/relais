package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/relaishq/relais/experiments/nvwa-room/client"
	"github.com/relaishq/relais/experiments/nvwa-room/harness"
	"github.com/relaishq/relais/experiments/nvwa-room/media"
	"github.com/relaishq/relais/experiments/nvwa-room/room"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func main() {
	bin := flag.String("bin", "", "binary directory")
	out := flag.String("out", "", "output directory")
	toneA := flag.String("tone-a", "", "440Hz Ogg")
	toneB := flag.String("tone-b", "", "880Hz Ogg")
	data := flag.Bool("datachannel", false, "same-PC data+audio transport")
	flag.Parse()
	if e := run(*bin, *out, *toneA, *toneB, *data); e != nil {
		log.Fatal(e)
	}
}
func run(bin, out, a, b string, data bool) error {
	root, _ := os.Getwd()
	bin, _ = filepath.Abs(bin)
	out, _ = filepath.Abs(out)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	top, e := harness.Start(ctx, bin, root, out, data)
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
	calls := make([]*media.Call, 2)
	clients := make([]*client.Client, 2)
	for i := range 2 {
		creds, e := client.Lobby(ctx, top.Config.RoomURL)
		if e != nil {
			return e
		}
		calls[i], e = media.Dial(ctx, signal, fmt.Sprintf("w%d", i+1), tones[i], tones[1-i], data)
		if e != nil {
			return e
		}
		defer calls[i].Close()
		var conn client.MessageConn
		if data {
			conn = calls[i].DataConn()
		} else {
			conn, e = client.DialWS(client.WSURL(top.Config.RoomURL))
		}
		if e != nil {
			return e
		}
		clients[i], e = client.Connect(ctx, conn, creds, calls[i].ID)
		if e != nil {
			return e
		}
		defer clients[i].Close()
	}
	time.Sleep(500 * time.Millisecond)
	pause, stop := context.WithTimeout(ctx, 80*time.Millisecond)
	_, pauseErr := clients[0].Request(pause, room.Message{Type: "test_pause", TestPauseMS: 2000})
	stop()
	if pauseErr == nil {
		return fmt.Errorf("consumer was not paused")
	}
	applied := 0
	for n := 0; n < 80; n++ {
		payload := json.RawMessage(`{"id":"wave","version":1,"start_tick":0}`)
		v, e := clients[1].Request(ctx, room.Message{Type: "trigger", CommandID: fmt.Sprintf("pressure-%d", n), InputSeq: uint64(n + 1), Payload: payload, Digest: room.Digest(payload)})
		if e != nil || v.Error != "" {
			return fmt.Errorf("producer command failure %v %s", e, v.Error)
		}
		applied++
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	observed := clients[0].Observations()
	typed := false
	for _, o := range observed {
		if o.Reply.Error == "slow_consumer" {
			typed = true
		}
	}
	var metrics room.Metrics
	res, e := http.Get(top.Config.RoomURL + "/metrics")
	if e != nil {
		return e
	}
	e = json.NewDecoder(res.Body).Decode(&metrics)
	res.Body.Close()
	if e != nil {
		return e
	}
	closedCtx, closedCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	_, closeErr := clients[0].Request(closedCtx, room.Message{Type: "snapshot"})
	closedCancel()
	reports := []media.AudioReport{calls[0].Report(), calls[1].Report()}
	result := map[string]any{"applied": applied, "typed_slow_consumer": typed, "client_close_error": fmt.Sprint(closeErr), "datachannel": data, "metrics": metrics, "audio": reports, "observations": observed}
	encoded, _ := json.MarshalIndent(result, "", "  ")
	if e = os.WriteFile(filepath.Join(out, "result.json"), encoded, 0600); e != nil {
		return e
	}
	fmt.Printf("applied=%d typed_slow_consumer=%t max_queue_bytes=%d max_queue_age_ms=%.3f audio_packets=%d/%d audio_max_gap_ms=%.3f/%.3f\n", applied, typed, metrics.MaxQueueBytes, metrics.MaxQueueAgeMS, reports[0].PeerPayload, reports[1].PeerPayload, reports[0].MaxGapMS, reports[1].MaxGapMS)
	if (!typed && (!data || closeErr == nil)) || metrics.SlowConsumers != 1 || metrics.MaxQueueBytes > room.MaxPending || metrics.Applied != uint64(applied) || reports[0].PeerPayload < 30 || reports[1].PeerPayload < 30 || reports[0].SelfPayload != 0 || reports[1].SelfPayload != 0 {
		return fmt.Errorf("backpressure acceptance failed")
	}
	return nil
}
