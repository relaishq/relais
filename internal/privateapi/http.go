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
	"strings"
	"time"

	"github.com/relais/pkg/sessionstore"
)

const MaxBody = 4 << 20
const Timeout = 2 * time.Second

// ErrUncertain means the request may have committed without a usable reply.
var ErrUncertain = errors.New("privateapi: operation outcome uncertain")

type uncertainError struct{ error }

func (e *uncertainError) Unwrap() error        { return e.error }
func (e *uncertainError) Is(target error) bool { return target == ErrUncertain }

// Uncertain preserves the transport cause, including context cancellation.
func Uncertain(err error) error { return &uncertainError{err} }

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

func Do(ctx context.Context, client *http.Client, base, method, path string, input, output any, codes map[string]error) error {
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
	if len(data) > MaxBody {
		return Uncertain(errors.New("privateapi: response too large"))
	}
	if resp.StatusCode/100 != 2 {
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
