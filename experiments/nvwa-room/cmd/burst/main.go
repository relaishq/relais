// burst probes the real transport with 200 numbered four-byte messages per
// client. It uses a bounded test-only echo lane, because the room contract
// intentionally admits at most 120 application messages per second.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/relaishq/relais/experiments/nvwa-room/client"
	"github.com/relaishq/relais/experiments/nvwa-room/harness"
	"github.com/relaishq/relais/experiments/nvwa-room/media"
	"github.com/relaishq/relais/experiments/nvwa-room/room"
)

type receipt struct {
	Seq   uint32  `json:"seq"`
	AtNS  int64   `json:"at_ns"`
	RTTMS float64 `json:"rtt_ms"`
}
type probe struct {
	mu             sync.Mutex
	SentNS         [200]int64        `json:"sent_ns"`
	Receipts       []receipt         `json:"receipts"`
	SendDurationMS float64           `json:"send_duration_ms"`
	Err            string            `json:"error,omitempty"`
	Before         int               `json:"peer_audio_before"`
	Audio          media.AudioReport `json:"audio"`
	Snapshot       room.Reply        `json:"snapshot"`
}

func (p *probe) receive(b []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(b) != 4 {
		p.Err = "bad response size"
		return
	}
	n := binary.BigEndian.Uint32(b)
	if n >= 200 {
		p.Err = "bad response sequence"
		return
	}
	p.Receipts = append(p.Receipts, receipt{n, time.Now().UnixNano(), float64(time.Now().UnixNano()-p.SentNS[n]) / 1e6})
}
func main() {
	bin := flag.String("bin", "", "binary directory")
	out := flag.String("out", "", "owned output directory")
	a := flag.String("tone-a", "", "440Hz Ogg")
	b := flag.String("tone-b", "", "880Hz Ogg")
	data := flag.Bool("datachannel", false, "shared-PC DataChannels")
	flag.Parse()
	if err := run(*bin, *out, *a, *b, *data); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run(bin, out, a, b string, data bool) error {
	root, _ := os.Getwd()
	bin, _ = filepath.Abs(bin)
	out, _ = filepath.Abs(out)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	top, err := harness.Start(ctx, bin, root, out, data)
	if err != nil {
		return err
	}
	defer top.Close()
	sig := &media.Signal{URL: top.Config.SignalURL, CertFile: top.Config.Cert, TokenFile: top.Config.Token}
	if err = sig.Init(); err != nil {
		return err
	}
	ta, err := media.LoadOpus(a)
	if err != nil {
		return err
	}
	tb, err := media.LoadOpus(b)
	if err != nil {
		return err
	}
	tones := [][][]byte{ta, tb}
	var calls [2]*media.Call
	var apps [2]*client.Client
	var probes [2]probe
	defer func() {
		for i := range 2 {
			if apps[i] != nil {
				_ = apps[i].Close()
			}
			if calls[i] != nil {
				calls[i].Close()
			}
		}
	}()
	var send [2]func([]byte) error
	var closeProbe [2]func() error
	for i := range 2 {
		creds, e := client.Lobby(ctx, top.Config.RoomURL)
		if e != nil {
			return e
		}
		calls[i], e = media.Dial(ctx, sig, fmt.Sprintf("w%d", i+1), tones[i], tones[1-i], data)
		if e != nil {
			return e
		}
		var conn client.MessageConn
		if data {
			conn = calls[i].DataConn()
		} else {
			conn, e = client.DialWS(client.WSURL(top.Config.RoomURL))
			if e != nil {
				return e
			}
		}
		apps[i], e = client.Connect(ctx, conn, creds, calls[i].ID)
		if e != nil {
			return e
		}
		if data {
			channel, e := calls[i].PC.CreateDataChannel("burst", nil)
			if e != nil {
				return e
			}
			defer channel.Close()
			closeProbe[i] = channel.Close
			channel.OnMessage(func(m webrtc.DataChannelMessage) { probes[i].receive(m.Data) })
			for channel.ReadyState() != webrtc.DataChannelStateOpen {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Millisecond):
				}
			}
			send[i] = channel.Send
		} else {
			conn, e := client.DialWS(strings.TrimSuffix(client.WSURL(top.Config.RoomURL), "/ws") + "/test-burst")
			// Use an explicit URL; no browser/profile or shared process is involved.
			if e != nil {
				return e
			}
			defer conn.Close()
			closeProbe[i] = conn.Close
			send[i] = conn.Send
			go func() {
				for {
					b, e := conn.Receive()
					if e != nil {
						return
					}
					probes[i].receive(b)
				}
			}()
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for calls[0].Report().PeerPayload < 20 || calls[1].Report().PeerPayload < 20 {
		if time.Now().After(deadline) {
			return fmt.Errorf("peer media startup timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var wg sync.WaitGroup
	for i := range 2 {
		probes[i].Before = calls[i].Report().PeerPayload
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			for n := range 200 {
				var payload [4]byte
				binary.BigEndian.PutUint32(payload[:], uint32(n))
				probes[i].mu.Lock()
				probes[i].SentNS[n] = time.Now().UnixNano()
				probes[i].mu.Unlock()
				if e := send[i](payload[:]); e != nil {
					probes[i].mu.Lock()
					probes[i].Err = e.Error()
					probes[i].mu.Unlock()
					return
				}
			}
			probes[i].mu.Lock()
			probes[i].SendDurationMS = float64(time.Since(start)) / 1e6
			probes[i].mu.Unlock()
		}()
	}
	wg.Wait()
	for i := range 2 {
		for {
			probes[i].mu.Lock()
			n, e := len(probes[i].Receipts), probes[i].Err
			probes[i].mu.Unlock()
			if e != "" {
				return fmt.Errorf("burst player%d: %s", i, e)
			}
			if n >= 200 && calls[i].Report().PeerPayload >= probes[i].Before+20 {
				break
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("burst player%d got %d: %w", i, n, ctx.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
		probes[i].mu.Lock()
		for n, r := range probes[i].Receipts {
			if r.Seq != uint32(n) {
				probes[i].Err = "duplicate/reordered receipt"
			}
		}
		probes[i].mu.Unlock()
		probes[i].Snapshot, err = apps[i].Request(ctx, room.Message{Type: "snapshot"})
		if err != nil {
			return err
		}
		if calls[i].PC.ConnectionState() != webrtc.PeerConnectionStateConnected {
			return fmt.Errorf("call ended after burst")
		}
		probes[i].Audio = calls[i].Report()
		if probes[i].Audio.SelfPayload != 0 || probes[i].Audio.UnknownPayload != 0 {
			return fmt.Errorf("not peer audio")
		}
	}
	// Close probe channels before collecting final queue peaks. Calls stay active.
	for i := range 2 {
		_ = closeProbe[i]()
		_ = apps[i].Close()
	}
	time.Sleep(100 * time.Millisecond)
	var metrics room.Metrics
	response, err := http.Get(top.Config.RoomURL + "/metrics")
	if err != nil {
		return err
	}
	err = json.NewDecoder(response.Body).Decode(&metrics)
	_ = response.Body.Close()
	if err != nil {
		return err
	}
	players := make([]any, 2)
	for i := range 2 {
		probes[i].mu.Lock()
		players[i] = map[string]any{"sent_ns": probes[i].SentNS, "receipts": probes[i].Receipts, "send_duration_ms": probes[i].SendDurationMS, "error": probes[i].Err, "peer_audio_before": probes[i].Before, "audio": probes[i].Audio, "snapshot": probes[i].Snapshot}
		probes[i].mu.Unlock()
		if probes[i].Err != "" {
			return fmt.Errorf("invalid numbered burst: %s", probes[i].Err)
		}
	}
	result := map[string]any{"datachannel": data, "messages_per_client": 200, "payload_bytes": 4, "lane": "bounded test-only transport echo; normal app limit remains 120/s", "players": players, "room_metrics": metrics}
	encoded, _ := json.MarshalIndent(result, "", "  ")
	if err = os.WriteFile(filepath.Join(out, "result.json"), encoded, 0600); err != nil {
		return err
	}
	fmt.Printf("datachannel=%v numbered_burst=200/200 per_client audio_survived=true app_snapshot_survived=true\n", data)
	return nil
}
