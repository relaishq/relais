package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
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

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/controlplane"
)

const maxRequestBytes = 64 << 10
const maxStatusBytes = 4 << 20
const requestTimeout = 10 * time.Second

// Match the demo's reverse-proxy pattern, exposing signaling
// operations and routed agent Control. Do not forward client credentials or trust forwarded headers.
func newHandler(target *url.URL, token string, transport http.RoundTripper) (http.Handler, error) {
	return newHandlerWithClock(target, token, transport, time.Now)
}

// Clock injection is local test tooling; production always uses time.Now.
func newHandlerWithClock(target *url.URL, token string, transport http.RoundTripper, now func() time.Time) (http.Handler, error) {
	var workerKey [32]byte
	if _, err := rand.Read(workerKey[:]); err != nil {
		return nil, err
	}
	expectedToken := sha256.Sum256([]byte(token))
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Header = http.Header{"Content-Type": pr.In.Header.Values("Content-Type")}
			pr.Out.Trailer = nil
			pr.Out.TransferEncoding = nil
			// Build a new query from the single validated public placement parameter.
			// Do not forward client query text or add X-Forwarded headers.
			pr.Out.URL.RawQuery = ""
			pr.Out.URL.ForceQuery = false
			if pr.In.Method == http.MethodPost {
				query := url.Values{}
				if worker := pr.In.URL.Query().Get("worker"); worker != "" {
					query.Set("worker", worker)
				}
				pr.Out.URL.RawQuery = query.Encode()
			}
		},
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
		if controlPath(r.Request.URL.Path) && r.StatusCode >= 400 && r.StatusCode <= 599 {
			var failure struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(data, &failure)
			message, known := controlErrors[failure.Code]
			if !known {
				failure.Code = "uncertain"
				message = "control request failed; retry the identical message"
			}
			data, _ = json.Marshal(struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}{failure.Code, message})
			headers.Set("Content-Type", "application/json")
		} else if r.StatusCode >= 400 && r.StatusCode <= 599 {
			data = []byte("signaling request failed; operation outcome may be uncertain\n")
			headers.Set("Content-Type", "text/plain; charset=utf-8")
		} else if controlPath(r.Request.URL.Path) && (r.StatusCode == http.StatusAccepted || r.StatusCode == http.StatusOK) {
			var receipt agent.ControlReceipt
			if json.Unmarshal(data, &receipt) != nil || receipt.ID == "" || receipt.Sequence == 0 || receipt.EventID == "" || receipt.Checkpointed != (r.StatusCode == http.StatusOK) {
				return errors.New("invalid control receipt")
			}
			data, err = json.Marshal(receipt)
			if err != nil {
				return err
			}
			headers.Set("Content-Type", "application/json")
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
		r.Header = headers
		r.Trailer = nil
		r.ContentLength = int64(len(data))
		r.Body = io.NopCloser(bytes.NewReader(data))
		return nil
	}
	client := &http.Client{Transport: transport, Timeout: requestTimeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received := now().UTC()
		w.Header().Set("Cache-Control", "no-store")
		auth := r.Header.Values("Authorization")
		presented := ""
		if len(auth) == 1 {
			scheme, value, _ := strings.Cut(auth[0], " ")
			if strings.EqualFold(scheme, "Bearer") {
				presented = value
			}
		}
		presentedToken := sha256.Sum256([]byte(presented))
		if len(auth) != 1 || subtle.ConstantTimeCompare(presentedToken[:], expectedToken[:]) != 1 {
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
		case controlPath(r.URL.Path):
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
		if !validQuery(r) {
			http.Error(w, "query refused", http.StatusBadRequest)
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
			writeJSON(w, clockSample{ReceivedAt: received, SentAt: now().UTC()})
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
			writeJSON(w, sanitizeStatus(status, workerKey[:]))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		r.ContentLength = int64(len(data))
		proxy.ServeHTTP(w, r)
	}), nil
}

func controlPath(path string) bool {
	base, ok := strings.CutSuffix(path, "/agent/control")
	return ok && callPath(base)
}

func callPath(path string) bool {
	id, ok := strings.CutPrefix(path, "/calls/")
	if !ok || id == "" || len(id) > 256 {
		return false
	}
	for _, c := range id {
		if !isIDChar(c) {
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
func workerID(name string, key []byte) string {
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
	hash := hmac.New(sha256.New, key)
	_, _ = hash.Write([]byte(name))
	return fmt.Sprintf("worker-%x", hash.Sum(nil)[:16])
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

func isIDChar(char rune) bool {
	return (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_'
}

func validQuery(r *http.Request) bool {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return false
	}
	for key, values := range query {
		if r.Method != http.MethodPost || r.URL.Path != "/calls" || key != "worker" || len(values) != 1 || !callPath("/calls/"+values[0]) {
			return false
		}
	}
	return true
}

// Only fixed public codes/messages may cross the trusted control boundary.
// Never reflect worker addresses, callback details or operator error strings.
var controlErrors = map[string]string{
	"control_invalid":     "invalid control message",
	"control_conflict":    "control ID or sequence conflicts",
	"control_order":       "control sequence must be next in order",
	"control_stale":       "control receipt expired; sequence already closed",
	"control_unsupported": "control requires a v2 agent",
	"agent_capacity":      "control capacity unavailable",
	"lease_lost":          "call ownership changed; retry the identical message",
	"unknown_call":        "unknown call", "unknown_session": "call route unavailable",
	"not_found": "call not found", "ended": "call ended",
	"move_in_progress": "call move in progress; retry the identical message",
	"transient":        "control temporarily unavailable; retry the identical message",
	"uncertain":        "control outcome uncertain; retry the identical message",
	"deadline":         "control deadline exceeded; retry the identical message",
	"canceled":         "control canceled; retry the identical message",
}
