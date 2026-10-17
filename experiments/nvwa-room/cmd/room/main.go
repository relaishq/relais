package main

import (
	"flag"
	"github.com/relaishq/relais/experiments/nvwa-room/media"
	"github.com/relaishq/relais/experiments/nvwa-room/room"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
)

func main() {
	addr := flag.String("http", "127.0.0.1:38690", "loopback HTTP")
	audio := flag.String("audio", "127.0.0.1:38691", "loopback UDP audio bus")
	web := flag.String("web", "experiments/nvwa-room/web", "browser page directory")
	signal := flag.String("signal", "", "signal HTTPS URL")
	cert := flag.String("signal-cert", "", "signal CA")
	token := flag.String("signal-token", "", "signal token file")
	privateNet := flag.String("spike-private-net", "", "spike-only owned namespace CIDR")
	networkState := flag.Bool("network-state-channel", false, "spike-only state broadcasts on state channel")
	faults := flag.Bool("test-faults", false, "allow receipt suppression")
	flag.Parse()
	for _, s := range []string{*addr, *audio} {
		a, e := netip.ParseAddrPort(s)
		prefix, _ := netip.ParsePrefix(*privateNet)
		if e != nil || (!a.Addr().IsLoopback() && !prefix.Contains(a.Addr())) {
			log.Fatal("loopback endpoints required")
		}
	}
	r := room.New("nvwa-spike")
	r.AllowFaults = *faults
	r.NetworkStateChannel = *networkState
	udp, e := net.ResolveUDPAddr("udp", *audio)
	if e != nil {
		log.Fatal(e)
	}
	conn, e := net.ListenUDP("udp", udp)
	if e != nil {
		log.Fatal(e)
	}
	defer conn.Close()
	go func() { _ = r.ServeAudio(conn) }()
	handler := r.Handler()
	if *signal != "" {
		s := &media.Signal{URL: *signal, CertFile: *cert, TokenFile: *token}
		if e = s.Init(); e != nil {
			log.Fatal(e)
		}
		handler = r.WithMediaProxy(handler, s)
	}
	mux := http.NewServeMux()
	mux.Handle("/", handler)
	mux.Handle("GET /web/", http.StripPrefix("/web/", http.FileServer(http.Dir(*web))))
	mux.HandleFunc("POST /browser-evidence", func(w http.ResponseWriter, q *http.Request) {
		if q.ContentLength > 1<<20 {
			http.Error(w, "too big", http.StatusRequestEntityTooLarge)
			return
		}
		f, e := os.CreateTemp("", "nvwa-browser-*.json")
		if e != nil {
			http.Error(w, e.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		q.Body = http.MaxBytesReader(w, q.Body, 1<<20)
		_, e = f.ReadFrom(q.Body)
		if e != nil {
			http.Error(w, e.Error(), http.StatusBadRequest)
			return
		}
		log.Printf("browser_evidence=%s", f.Name())
		w.WriteHeader(http.StatusOK)
	})
	log.Printf("room_http=%s room_epoch=%s audio_udp=%s", *addr, r.Epoch, *audio)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
