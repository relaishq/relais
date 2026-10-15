// Package privateapi provides bounded JSON transport for trusted loopback APIs.
// Requests that mutate media state are never retried by this transport.
package privateapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/movediag"
	"github.com/relaishq/relais/pkg/metrics"
	"github.com/relaishq/relais/pkg/sessionstore"
)

const MaxBody = 4 << 20
const Timeout = 2 * time.Second

// ErrorCodes returns all matching sentinel codes for an operator result envelope.
// Uncertainty is explicit because its text may contain only the transport cause.
func ErrorCodes(err error, codes map[string]error) []string {
	matched := make(map[string]bool)
	for code, sentinel := range CommonErrors {
		if errors.Is(err, sentinel) {
			matched[code] = true
		}
	}
	for code, sentinel := range codes {
		if errors.Is(err, sentinel) {
			matched[code] = true
		}
	}
	if errors.Is(err, ErrUncertain) {
		matched["uncertain"] = true
	}
	names := make([]string, 0, len(matched))
	for code := range matched {
		names = append(names, code)
	}
	sort.Strings(names)
	return names
}

type envelopeError struct {
	message string
	causes  []error
}

func (e *envelopeError) Error() string   { return e.message }
func (e *envelopeError) Unwrap() []error { return e.causes }

// ErrUncertain means the request may have committed without a usable reply.
var ErrUncertain = errors.New("privateapi: operation outcome uncertain")

type uncertainError struct{ error }

func (e *uncertainError) Unwrap() error        { return e.error }
func (e *uncertainError) Is(target error) bool { return target == ErrUncertain }

// Uncertain preserves the transport cause, including context cancellation.
func Uncertain(err error) error { return &uncertainError{err} }

// HTTPError preserves the response status alongside the decoded API cause.
type HTTPError struct {
	StatusCode int
	Err        error
}

func (e *HTTPError) Error() string { return e.Err.Error() }
func (e *HTTPError) Unwrap() error { return e.Err }

type Failure struct {
	Code      string              `json:"code"`
	Message   string              `json:"message"`
	Candidate *sessionstore.Lease `json:"candidate,omitempty"`
}

var CommonErrors = map[string]error{
	"canceled":   context.Canceled,
	"deadline":   context.DeadlineExceeded,
	"not_found":  sessionstore.ErrNotFound,
	"lease_lost": sessionstore.ErrLeaseLost,
	"transient":  sessionstore.ErrTransient,
}

func Write(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func Error(w http.ResponseWriter, err error, codes map[string]error) {
	f := Failure{Code: "internal", Message: err.Error()}
	for code, sentinel := range CommonErrors {
		if errors.Is(err, sentinel) {
			f.Code = code
			break
		}
	}
	for code, sentinel := range codes {
		if errors.Is(err, sentinel) {
			f.Code = code
			break
		}
	}
	var transient *sessionstore.TransientError
	if errors.As(err, &transient) {
		f.Code, f.Candidate = "transient", transient.Candidate
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	Write(w, f)
}

func Read(w http.ResponseWriter, r *http.Request, value any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		http.Error(w, "invalid JSON request", http.StatusBadRequest)
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		http.Error(w, "expected one JSON request", http.StatusBadRequest)
		return false
	}
	return true
}

// Do keeps the peer API's code/message error decoding. An explicit error
// response is never classified as uncertain because of an operator envelope.
func Do(ctx context.Context, client *http.Client, base, method, path string, input, output any, codes map[string]error) error {
	return do(ctx, client, base, method, path, input, output, codes, false)
}

// DoResult opts an operator move client into error/codes/result decoding.
// It fills output on failure and preserves every recognized error sentinel.
func DoResult(ctx context.Context, client *http.Client, base, method, path string, input, output any, codes map[string]error) error {
	return do(ctx, client, base, method, path, input, output, codes, true)
}

func do(ctx context.Context, client *http.Client, base, method, path string, input, output any, codes map[string]error, resultEnvelope bool) (resultErr error) {
	if client.Transport == nil {
		copy := *client
		copy.Transport = metrics.HTTPTransport()
		client = &copy
	}
	var body bytes.Buffer
	if input != nil {
		if err := json.NewEncoder(&body).Encode(input); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, &body)
	if err != nil {
		return err
	}

	var diagnostic *requestDiagnostic
	if movediag.Enabled() && movediag.ID(ctx) != "" {
		req, diagnostic = traceRequest(req, path, body.Len())
		defer func() { diagnostic.finish(resultErr != nil) }()
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Uncertain(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return Uncertain(err)
	}
	if diagnostic != nil {
		diagnostic.responseBytes(len(data))
	}
	if len(data) > MaxBody {
		return Uncertain(errors.New("privateapi: response too large"))
	}
	if resp.StatusCode/100 != 2 {
		defer func() { resultErr = &HTTPError{StatusCode: resp.StatusCode, Err: resultErr} }()
		if resultEnvelope {
			var envelope struct {
				Codes  []string        `json:"codes"`
				Error  string          `json:"error"`
				Result json.RawMessage `json:"result"`
			}
			if json.Unmarshal(data, &envelope) == nil && envelope.Error != "" {
				failure := &envelopeError{message: envelope.Error}
				for _, code := range envelope.Codes {
					cause := codes[code]
					if cause == nil {
						cause = CommonErrors[code]
					}
					if code == "uncertain" {
						cause = ErrUncertain
					}
					if cause != nil {
						failure.causes = append(failure.causes, cause)
					}
				}
				if output != nil && len(envelope.Result) > 0 && string(envelope.Result) != "null" {
					if err := json.Unmarshal(envelope.Result, output); err != nil {
						return errors.Join(failure, Uncertain(err))
					}
				}
				return failure
			}
		}
		var failure Failure
		if json.Unmarshal(data, &failure) != nil || failure.Message == "" {
			return fmt.Errorf("privateapi: %s: %s", resp.Status, strings.TrimSpace(string(data)))
		}
		if failure.Code == "transient" {
			return &sessionstore.TransientError{Op: "remote", Err: errors.New(failure.Message), Candidate: failure.Candidate}
		}
		cause := codes[failure.Code]
		if cause == nil {
			cause = CommonErrors[failure.Code]
		}
		if cause == nil {
			return errors.New(failure.Message)
		}
		return fmt.Errorf("%s: %w", failure.Message, cause)
	}
	if output == nil {
		return nil
	}
	if err := json.Unmarshal(data, output); err != nil {
		return Uncertain(err)
	}
	return nil
}

// DiagnosticOperation records only operation names. Session/worker identifiers stay private.
func DiagnosticOperation(path string) string {
	if strings.HasPrefix(path, "/sessions/") {
		for _, op := range []string{"export", "hold", "move", "release", "move-after-export", "release-after-export", "keyframe", "begin-replay", "replay"} {
			if strings.HasSuffix(path, "/"+op) {
				return "/sessions/:id/" + op
			}
		}
		return "/sessions/:id"
	}
	switch path {
	case "/resume", "/status", "/sessions", "/workers":
		return path
	}
	return "/private-operation"
}
