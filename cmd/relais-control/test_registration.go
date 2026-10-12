package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/controlplane"
)

type testPortRange struct{ first, last uint16 }

func parseTestPortRange(s string) (testPortRange, error) {
	first, last, ok := strings.Cut(s, "-")
	if !ok {
		return testPortRange{}, errors.New("port range must be first-last")
	}
	lo, err := strconv.ParseUint(first, 10, 16)
	if err != nil || lo == 0 {
		return testPortRange{}, errors.New("first port must be 1-65535")
	}
	hi, err := strconv.ParseUint(last, 10, 16)
	if err != nil || hi < lo {
		return testPortRange{}, errors.New("last port must be first-65535")
	}
	return testPortRange{first: uint16(lo), last: uint16(hi)}, nil
}

func (p testPortRange) contains(port uint16) bool { return port >= p.first && port <= p.last }

type testWorkerPorts struct{ http, udp testPortRange }
type testWorkerAllowlist map[netip.Addr]testWorkerPorts

func (a *testWorkerAllowlist) String() string {
	var entries []string
	for ip, ports := range *a {
		entries = append(entries, fmt.Sprintf("%s=%d-%d,%d-%d", ip, ports.http.first, ports.http.last, ports.udp.first, ports.udp.last))
	}
	slices.Sort(entries)
	return strings.Join(entries, ";")
}

// Set admits exact RFC 1918 or unique-local addresses, never DNS names or CIDRs.
// Each host has its own inclusive HTTP and UDP bounds. Invalid flags fail startup.
func (a *testWorkerAllowlist) Set(s string) error {
	host, ranges, ok := strings.Cut(s, "=")
	ip, err := netip.ParseAddr(host)
	if !ok || err != nil || !ip.IsPrivate() || ip.Zone() != "" || ip.Is4In6() {
		return errors.New("test-register-worker requires a literal private IP=HTTP-first-last,UDP-first-last")
	}
	httpRange, udpRange, ok := strings.Cut(ranges, ",")
	if !ok {
		return errors.New("test-register-worker requires both HTTP and UDP port ranges")
	}
	httpPorts, err := parseTestPortRange(httpRange)
	if err != nil {
		return fmt.Errorf("test-register-worker HTTP: %w", err)
	}
	udpPorts, err := parseTestPortRange(udpRange)
	if err != nil {
		return fmt.Errorf("test-register-worker UDP: %w", err)
	}
	if _, exists := (*a)[ip]; exists {
		return errors.New("test-register-worker: duplicate host")
	}
	if *a == nil {
		*a = make(testWorkerAllowlist)
	}
	(*a)[ip] = testWorkerPorts{http: httpPorts, udp: udpPorts}
	return nil
}

type testRegistrationRequest struct {
	Name    string         `json:"name"`
	URL     string         `json:"url"`
	Address netip.AddrPort `json:"address"`
}

func (a testWorkerAllowlist) validate(req testRegistrationRequest) error {
	u, err := url.Parse(req.URL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || strings.TrimSpace(req.Name) == "" {
		return errors.New("name, literal HTTP worker URL and media address required")
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return errors.New("worker URL must have a literal host and explicit port")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return errors.New("worker URL host must be an allowed private IP")
	}
	ports, allowed := a[ip]
	httpPort, err := strconv.ParseUint(port, 10, 16)
	if !allowed || err != nil || !ports.http.contains(uint16(httpPort)) {
		return errors.New("worker HTTP host or port is not allowed")
	}
	if !req.Address.IsValid() || req.Address.Addr() != ip || !ports.udp.contains(req.Address.Port()) {
		return errors.New("worker media address must use the same allowed host and UDP port range")
	}
	return nil
}

// testWorkerClient keeps every request to the validated literal endpoint.
// Environment proxies and redirects must not widen the configured host bounds.
func testWorkerClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{
		Timeout:   privateapi.Timeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// controlHandler preserves demo defaults. The separate test route is absent
// without an allowlist. Like other private control routes, it has no auth.
func controlHandler(plane *controlplane.Plane, relay *controlplane.RemoteRelay, demo bool, allowed testWorkerAllowlist, client *http.Client) http.Handler {
	base := plane.ProcessHandler()
	if demo {
		base = demoHandler(plane, relay)
	}
	if len(allowed) == 0 {
		return base
	}
	mux := http.NewServeMux()
	mux.Handle("/", base)
	mux.HandleFunc("POST /private/test/register", func(w http.ResponseWriter, r *http.Request) {
		var req testRegistrationRequest
		if !privateapi.Read(w, r, &req) {
			return
		}
		if err := allowed.validate(req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		worker := &controlplane.RemoteWorker{URL: req.URL, Client: client}
		status, err := worker.Status(r.Context())
		if err == nil && status.Address != req.Address {
			err = errors.New("worker status does not match requested media address")
		}
		if err == nil {
			// The existing relay-sync loop discovers accepted registrations.
			// Register first so rejected identities never widen the relay registry.
			err = plane.Register(req.Name, req.Address, worker)
		}
		if err != nil {
			privateapi.Error(w, fmt.Errorf("test registration: %w", err), nil)
			return
		}
		privateapi.Write(w, map[string]any{"name": req.Name, "address": req.Address})
	})
	return mux
}
