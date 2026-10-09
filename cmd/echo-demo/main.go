// Command echo-demo is the browser demo for the media worker. It runs one
// media worker and serves a page that calls it: the page publishes camera
// and microphone (or a generated test pattern) through the worker's
// WHIP-style signaling endpoint, plays the echo, and exposes WebRTC stats for
// automated checks (window.relaisStats and the #stats element).
//
// Browsers allow getUserMedia only in a secure context, which
// http://localhost is, so the page is meant to be opened on localhost.
// Chrome does not gather loopback ICE candidates, so it cannot reach a
// worker on 127.0.0.1. The worker's media socket, which is also the single
// host candidate in every answer, therefore defaults to this machine's
// primary LAN IPv4 address.
//
// With -relay, a relay owns that media address instead and the worker sits
// behind it on a private loopback socket: callers see only the relay's
// address, and the page works the same.
//
// Usage:
//
//	go run ./cmd/echo-demo [-http localhost:9101] [-media-ip 192.168.1.20] [-media-port 9150] [-relay]
//
// Each flag can also be set in the environment: RELAIS_DEMO_HTTP_ADDR,
// RELAIS_DEMO_MEDIA_IP, RELAIS_DEMO_MEDIA_PORT and RELAIS_DEMO_RELAY=1. Then open
// http://localhost:9101/ for camera and microphone, or
// http://localhost:9101/?source=test for the test pattern (no permission
// prompt).
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/pion/logging"
	"github.com/relais/pkg/mediaworker"
)

const (
	defaultHTTPAddr  = "localhost:9101"
	defaultMediaPort = 9150
)

//go:embed web
var webFiles embed.FS

func main() {
	if err := run(); err != nil {
		log.Fatalf("echo-demo: %v", err)
	}
}

func run() error {
	defaultPort, err := envInt("RELAIS_DEMO_MEDIA_PORT", defaultMediaPort)
	if err != nil {
		return err
	}
	httpAddr := flag.String("http", envString("RELAIS_DEMO_HTTP_ADDR", defaultHTTPAddr),
		"TCP address for the page and the signaling endpoint; keep it on localhost (a secure context for getUserMedia)")
	mediaIP := flag.String("media-ip", os.Getenv("RELAIS_DEMO_MEDIA_IP"),
		"IP of the worker's UDP media socket, advertised as the single host candidate (default: this machine's primary LAN IPv4)")
	mediaPort := flag.Int("media-port", defaultPort, "UDP port of the worker's media socket (0 picks a free port)")
	withRelay := flag.Bool("relay", os.Getenv("RELAIS_DEMO_RELAY") == "1",
		"put a relay on the media address and the worker behind it on a private loopback socket")
	flag.Parse()
	if *mediaPort < 0 || *mediaPort > 65535 {
		return fmt.Errorf("-media-port %d out of range", *mediaPort)
	}

	ip, err := mediaAddress(*mediaIP)
	if err != nil {
		return err
	}

	media, err := startMedia(netip.AddrPortFrom(ip, uint16(*mediaPort)), *withRelay, loggerFactory())
	if err != nil {
		return err
	}
	defer func() { _ = media.Close() }()

	listener, err := net.Listen("tcp", *httpAddr)
	if err != nil {
		return fmt.Errorf("listen for HTTP: %w", err)
	}
	server := &http.Server{Handler: newHandler(media.worker), ReadHeaderTimeout: 5 * time.Second}

	_, port, _ := net.SplitHostPort(listener.Addr().String())
	log.Printf("echo-demo: camera and microphone: http://localhost:%s/", port)
	log.Printf("echo-demo: test pattern:          http://localhost:%s/?source=test", port)
	if host, _, _ := net.SplitHostPort(*httpAddr); host != "localhost" && host != "127.0.0.1" && host != "::1" {
		log.Printf("echo-demo: warning: browsers allow camera and microphone only on localhost or HTTPS")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}

	log.Printf("echo-demo: shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return server.Shutdown(shutdownCtx)
}

// newHandler serves the demo page and the worker's signaling endpoint
// (POST /calls, DELETE /calls/{id}) from one origin.
func newHandler(worker *mediaworker.Worker) http.Handler {
	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err) // the embedded directory is fixed at build time
	}
	files := http.FileServerFS(static)

	mux := http.NewServeMux()
	signaling := worker.SignalingHandler()
	mux.Handle(mediaworker.CallsPath, signaling)
	mux.Handle(mediaworker.CallsPath+"/", signaling)
	mux.Handle("/", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// Always serve the current page while iterating on it.
		rw.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(rw, r)
	}))

	return mux
}

// mediaAddress returns the IP for the worker's media socket: the flag value
// if set, otherwise the primary LAN IPv4 address.
func mediaAddress(flagValue string) (netip.Addr, error) {
	if flagValue != "" {
		ip, err := netip.ParseAddr(flagValue)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("-media-ip: %w", err)
		}
		if ip.IsLoopback() {
			log.Printf("echo-demo: warning: Chrome does not gather loopback candidates and cannot reach %s", ip)
		}

		return ip, nil
	}

	ip, err := primaryIPv4()
	if err != nil {
		return netip.Addr{}, err
	}

	return ip, nil
}

// primaryIPv4 returns the IPv4 address this machine uses for its default
// route; a browser on the same machine reaches the worker there. Dialing UDP
// sends nothing: it only makes the OS pick a source address. Without a
// default route it falls back to the first non-loopback IPv4 address of an
// interface that is up.
func primaryIPv4() (netip.Addr, error) {
	if conn, err := net.Dial("udp4", "192.0.2.1:9"); err == nil { // TEST-NET-1, never contacted
		local, ok := conn.LocalAddr().(*net.UDPAddr)
		_ = conn.Close()
		if ok {
			if ip, ok := netip.AddrFromSlice(local.IP); ok && usable(ip.Unmap()) {
				return ip.Unmap(), nil
			}
		}
	}

	interfaces, err := net.Interfaces()
	if err != nil {
		return netip.Addr{}, err
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			prefix, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if ip, ok := netip.AddrFromSlice(prefix.IP); ok && ip.Unmap().Is4() && usable(ip.Unmap()) {
				return ip.Unmap(), nil
			}
		}
	}

	return netip.Addr{}, errors.New("no non-loopback IPv4 address found; pass -media-ip")
}

func usable(ip netip.Addr) bool {
	return ip.Is4() && !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsLinkLocalUnicast()
}

// loggerFactory logs the worker's own session events and the relay's routing
// at Info and Pion's components at Error, unless PION_LOG_* says otherwise.
func loggerFactory() logging.LoggerFactory {
	factory := logging.NewDefaultLoggerFactory()
	for _, scope := range []string{"mediaworker", "session", "relay"} {
		if _, set := factory.ScopeLevels[scope]; !set && factory.DefaultLogLevel < logging.LogLevelInfo {
			factory.ScopeLevels[scope] = logging.LogLevelInfo
		}
	}

	return factory
}

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}

func envInt(name string, fallback int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}

	return n, nil
}
