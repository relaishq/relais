package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/relais/pkg/controlplane"
)

const maxRequestBytes = 64 << 10
const maxStatusBytes = 4 << 20
const requestTimeout = 10 * time.Second

// Match the demo's reverse-proxy pattern, but expose only the two signaling
// operations. Do not forward client credentials or trust forwarded headers.
func newHandler(target *url.URL, token string, transport http.RoundTripper) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport
	director := proxy.Director
	proxy.Director = func(r *http.Request) {
		director(r)
		r.Host = target.Host
		r.Trailer = nil
		r.TransferEncoding = nil
		r.Header = http.Header{"Content-Type": r.Header.Values("Content-Type")}
		// Suppress ReverseProxy's automatic X-Forwarded-For insertion.
		r.Header["X-Forwarded-For"] = nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "signaling unavailable; operation outcome may be uncertain", http.StatusBadGateway)
	}
	proxy.ModifyResponse = func(r *http.Response) error {
		originalBody := r.Body
		defer func() { _ = originalBody.Close() }()
		data, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
		if err != nil || len(data) > maxRequestBytes {
			return errors.New("invalid signaling response")
		}
		headers := make(http.Header)
		if r.StatusCode >= 400 && r.StatusCode <= 599 {
			data = []byte("signaling request failed; operation outcome may be uncertain\n")
			headers.Set("Content-Type", "text/plain; charset=utf-8")
		} else if r.Request.Method == http.MethodPost && r.StatusCode == http.StatusCreated {
			location := r.Header.Get("Location")
			if !callPath(location) {
				return errors.New("invalid call location")
			}
			headers.Set("Location", location)
			headers.Set("Content-Type", "application/sdp")
		} else if r.Request.Method == http.MethodDelete && r.StatusCode == http.StatusOK {
			data = nil
		} else {
			return errors.New("unexpected signaling response")
		}
		headers.Set("Cache-Control", "no-store")
		r.Header = headers
		r.Trailer = nil
		r.ContentLength = int64(len(data))
		r.Body = io.NopCloser(bytes.NewReader(data))
		return nil
	}
	client := &http.Client{Transport: transport, Timeout: requestTimeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received := time.Now().UTC()
		w.Header().Set("Cache-Control", "no-store")
		auth := r.Header.Values("Authorization")
		if len(auth) != 1 || subtle.ConstantTimeCompare([]byte(auth[0]), []byte("Bearer "+token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "bearer token required", http.StatusUnauthorized)
			return
		}
		// Dispatch without ServeMux's path cleaning, redirects, or GET/HEAD alias.
		// Encoded slashes/dot segments cannot become another private API route.
		if r.URL.RawPath != "" || r.URL.EscapedPath() != r.URL.Path {
			http.NotFound(w, r)
			return
		}
		method := ""
		switch {
		case r.URL.Path == "/calls":
			method = http.MethodPost
		case callPath(r.URL.Path):
			method = http.MethodDelete
		case r.URL.Path == "/status" || r.URL.Path == "/clock":
			method = http.MethodGet
		default:
			http.NotFound(w, r)
			return
		}
		if r.Method != method {
			w.Header().Set("Allow", method)
			http.Error(w, "method refused", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			code := http.StatusBadRequest
			if errors.As(err, &tooLarge) {
				code = http.StatusRequestEntityTooLarge
			}
			http.Error(w, "cannot read request", code)
			return
		}
		if r.URL.Path == "/clock" {
			writeJSON(w, clockSample{ReceivedAt: received, SentAt: time.Now().UTC()})
			return
		}
		if r.URL.Path == "/status" {
			statusURL := *target
			statusURL.Path = "/status"
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL.String(), nil)
			if err != nil {
				http.Error(w, "status unavailable", http.StatusBadGateway)
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				http.Error(w, "status unavailable", http.StatusBadGateway)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(io.LimitReader(resp.Body, maxStatusBytes+1))
			var status controlplane.Status
			if err != nil || len(body) > maxStatusBytes || resp.StatusCode != http.StatusOK || json.Unmarshal(body, &status) != nil || !validStatus(status) {
				http.Error(w, "status unavailable", http.StatusBadGateway)
				return
			}
			writeJSON(w, sanitizeStatus(status))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		r.ContentLength = int64(len(data))
		proxy.ServeHTTP(w, r)
	})
}

func callPath(path string) bool {
	id, ok := strings.CutPrefix(path, "/calls/")
	if !ok || id == "" || len(id) > 256 {
		return false
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func upstreamTransport() *http.Transport {
	return &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:    3 * time.Second,
		ResponseHeaderTimeout:  requestTimeout,
		IdleConnTimeout:        30 * time.Second,
		MaxIdleConns:           32,
		MaxIdleConnsPerHost:    32,
		MaxResponseHeaderBytes: 32 << 10,
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

type clockSample struct {
	ReceivedAt time.Time `json:"received_at"`
	SentAt     time.Time `json:"sent_at"`
}

// Numeric worker generations stay compatible with caller polling. Other names
// become stable opaque identifiers, so an address used as a name cannot leak.
func workerID(name string) string {
	if name == "" {
		return ""
	}
	numeric := true
	for _, c := range name {
		if c < '0' || c > '9' {
			numeric = false
			break
		}
	}
	if numeric {
		return name
	}
	hash := sha256.Sum256([]byte(name))
	return fmt.Sprintf("worker-%x", hash[:16])
}

// Missing status collections are unavailable state, not proof of zero calls.
// Public call IDs follow the worker's URL-safe signaling resource grammar.
func validStatus(status controlplane.Status) bool {
	if status.Workers == nil || status.Calls == nil || status.Takeovers == nil {
		return false
	}
	for _, c := range status.Calls {
		if !callPath("/calls/" + c.ID) {
			return false
		}
	}
	for _, e := range status.Takeovers {
		if !callPath("/calls/" + e.ID) {
			return false
		}
	}
	return true
}
