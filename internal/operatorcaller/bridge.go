package operatorcaller

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/operator"
)

type BridgeConfig struct {
	Identity        Identity `json:"identity"`
	Bind            string   `json:"bind"`
	CertificateFile string   `json:"certificateFile"`
	LoadSHA256      string   `json:"loadSHA256"`
}

func OperatorURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Port() == "80" || u.User != nil || (u.Path != "/" && u.Path != "") || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("caller-operator-not-loopback")
	}
	return u.Scheme + "://" + u.Host, nil
}
func SignalRTTs(ctx context.Context, signaling, token string, client *http.Client) []int64 {
	endpoint := strings.TrimSuffix(signaling, "/calls") + "/clock"
	var rtts []int64
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for range ClockBurstCount {
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		req, err := http.NewRequestWithContext(probe, http.MethodGet, endpoint, nil)
		if err != nil {
			cancel()
			return nil
		}
		req.Header.Set("Authorization", "Bearer "+token)
		start := time.Now()
		r, err := clientCopy.Do(req)
		if err != nil {
			cancel()
			return nil
		}
		_, err = io.Copy(io.Discard, io.LimitReader(r.Body, operator.MaxFrameBytes+1))
		_ = r.Body.Close()
		cancel()
		if err != nil || r.StatusCode != http.StatusOK {
			return nil
		}
		rtts = append(rtts, max(1, time.Since(start).Nanoseconds()))
	}
	return rtts
}

// Shutdown shares one grace timer across every abort route.
type Shutdown struct {
	mu                sync.Mutex
	requested, exited bool
	timer             *time.Timer
	signal            func(os.Signal) error
}

func NewShutdown(signal func(os.Signal) error) *Shutdown { return &Shutdown{signal: signal} }
func (s *Shutdown) Interrupt(remaining time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited || s.requested {
		return
	}
	s.requested = true
	_ = s.signal(os.Interrupt)
	s.timer = time.AfterFunc(min(ReportGrace, max(0, remaining)), func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.exited {
			_ = s.signal(syscall.SIGKILL)
		}
	})
}
func (s *Shutdown) Exited() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exited = true
	if s.timer != nil {
		s.timer.Stop()
	}
}
func waitManifest(ctx context.Context, i Identity, box Store, end time.Time) (Manifest, error) {
	for time.Now().Before(end) {
		raw, err := box.Read(ctx, i.Root()+"/transport.json")
		if err != nil {
			return Manifest{}, err
		}
		if raw.Found {
			var m Manifest
			if err = decode(raw.Data, &m); err != nil {
				return m, err
			}
			return m, m.Validate(i, time.Now())
		}
		stopped, err := Stopped(ctx, box, i)
		if err != nil {
			return Manifest{}, err
		}
		if stopped {
			return Manifest{}, errors.New("conductor-stopped")
		}
		if err = sleep(ctx, min(500*time.Millisecond, time.Until(end))); err != nil {
			return Manifest{}, err
		}
	}
	return Manifest{}, errors.New("transport-deadline")
}

// RunBridge runs the unchanged child and publishes only create-only evidence.
// The launcher supplies the validated configuration, TLS CA and exact load hash.
func RunBridge(ctx context.Context, c BridgeConfig, box Store, load string) error {
	if err := c.Identity.Validate(); err != nil {
		return err
	}
	host, hostErr := CollectHost(ctx)
	if hostErr != nil {
		return hostErr
	}
	if !digestPattern.MatchString(c.LoadSHA256) {
		return errors.New("invalid-load-hash")
	}
	binary, err := os.ReadFile(load)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(binary)
	if hex.EncodeToString(sum[:]) != c.LoadSHA256 {
		return errors.New("load-hash-differs")
	}
	m, err := waitManifest(ctx, c.Identity, box, time.Now().Add(20*time.Minute))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(ctx, time.UnixMilli(m.Deadline))
	defer cancel()
	ca, err := os.ReadFile(c.CertificateFile)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return errors.New("invalid-caller-ca")
	}
	signalClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}, Timeout: 5 * time.Second}
	defer signalClient.CloseIdleConnections()
	raw, err := box.Read(ctx, m.Root()+"/caller-token.json")
	if err != nil {
		return err
	}
	var token struct {
		Bearer string `json:"bearerToken"`
	}
	if !raw.Found || decode(raw.Data, &token) != nil || !tokenPattern.MatchString(token.Bearer) {
		return errors.New("invalid-caller-token")
	}
	dir, err := os.MkdirTemp("", "relais-caller-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	tokenFile := filepath.Join(dir, "caller.token")
	if err = os.WriteFile(tokenFile, []byte(token.Bearer), 0600); err != nil {
		return err
	}
	if createJSON(ctx, box, m.Job()+"/host.json", host) != Committed {
		return errors.New("host-publication-unknown")
	}
	setup := Setup{SchemaVersion: 1, RunID: m.RunID, PublicSHA: m.PublicSHA, Job: "caller-1", CallerID: m.CallerID, Attempt: m.Attempt, RuleAddedAt: time.UnixMilli(m.CreatedAt), SignalRTTsNS: SignalRTTs(ctx, m.Endpoints.Signaling, token.Bearer, signalClient)}
	if err = AwaitBegin(ctx, m, box, SystemClock{}, setup); err != nil {
		return err
	}
	endpoints := m.Endpoints
	endpoints.Bind = c.Bind
	endpoints.TokenFile = tokenFile
	args, err := CallerArgs(m.Plan, endpoints)
	if err != nil {
		return err
	}
	report := filepath.Join(dir, "caller.jsonl")
	args = append(args, "-signaling-cert="+c.CertificateFile, "-output="+report, fmt.Sprint("-parent-pid=", os.Getpid()))
	child := exec.Command(load, args...)
	child.Env = PublicEnvironment(os.Environ())
	reader, writer := io.Pipe()
	child.Stdout = writer
	child.Stderr = io.Discard
	if err = child.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return err
	}
	shutdown := NewShutdown(child.Process.Signal)
	exited := make(chan struct{})
	var exitErr error
	go func() { exitErr = child.Wait(); shutdown.Exited(); _ = writer.Close(); close(exited) }()
	var stateMu sync.Mutex
	stopped := false
	done := false
	stats := 0
	httpURL := ""
	var sessions []string
	// Publishing a stop is one submission even when publication is lost.
	stop := func(reason string) error {
		stateMu.Lock()
		defer stateMu.Unlock()
		if stopped {
			return nil
		}
		stopped = true
		if createJSON(context.WithoutCancel(ctx), box, m.Job()+"/stop.json", map[string]string{"runId": m.RunID, "job": "caller-1", "reason": reason}) != Committed {
			return errors.New("stop-publication-unknown")
		}
		return nil
	}
	interrupt := func() { shutdown.Interrupt(time.Until(time.UnixMilli(m.Deadline))) }
	localClient := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer localClient.CloseIdleConnections()
	local := func(callCtx context.Context, path string, body, result any) error {
		stateMu.Lock()
		base := httpURL
		stateMu.Unlock()
		var input io.Reader
		method := http.MethodGet
		if body != nil {
			data, e := json.Marshal(body)
			if e != nil {
				return e
			}
			input = bytes.NewReader(data)
			method = http.MethodPost
		}
		callCtx, stopCall := context.WithTimeout(callCtx, 10*time.Second)
		defer stopCall()
		req, e := http.NewRequestWithContext(callCtx, method, base+path, input)
		if e != nil {
			return e
		}
		req.Header.Set("Content-Type", "application/json")
		response, e := localClient.Do(req)
		if e != nil {
			return e
		}
		defer response.Body.Close()
		data, e := io.ReadAll(io.LimitReader(response.Body, operator.MaxBlobBytes+1))
		if e != nil {
			return e
		}
		if len(data) > operator.MaxBlobBytes || response.StatusCode < 200 || response.StatusCode >= 300 {
			return errors.New("caller-local-rejected")
		}
		if result != nil && len(data) > 0 {
			return decode(data, result)
		}
		return nil
	}
	deliveryCtx, cancelDelivery := context.WithCancel(ctx)
	defer cancelDelivery()
	streamFailed := make(chan struct{})
	hungUp := make(chan struct{})
	stream := make(chan error, 1)
	go func() {
		w := SaturationWindow{PlannedSeconds: float64(m.Plan.DurationSeconds)}
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 4096), operator.MaxBlobBytes)
		scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
			if atEOF && len(data) > 0 && !bytes.ContainsRune(data, '\n') {
				return 0, nil, errors.New("truncated-caller-stdout")
			}
			return bufio.ScanLines(data, atEOF)
		})
		var streamErr error
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			var msg loadgen.Message
			if e := decode(line, &msg); e != nil {
				streamErr = e
				break
			}
			switch msg.Type {
			case "ready":
				var ready struct {
					HTTP    string `json:"http"`
					Callers int    `json:"callers"`
				}
				_ = decode(line, &ready)
				address, e := OperatorURL(ready.HTTP)
				if e != nil {
					streamErr = e
					break
				}
				var ids []string
				if e = localWithBase(ctx, address, localClient, "/sessions", &ids); e != nil {
					streamErr = e
					break
				}
				unique := map[string]bool{}
				for _, s := range ids {
					unique[s] = true
				}
				stateMu.Lock()
				duplicate := httpURL != ""
				stateMu.Unlock()
				if duplicate || ready.Callers != m.Plan.Calls || len(ids) != m.Plan.Calls || len(unique) != len(ids) || msg.PID != child.Process.Pid {
					streamErr = errors.New("caller-readiness-count")
					break
				}
				stateMu.Lock()
				httpURL = address
				sessions = ids
				stateMu.Unlock()
				var probes json.RawMessage
				if e = local(ctx, "/clock-probes", nil, &probes); e != nil {
					streamErr = e
					break
				}
				if createJSON(ctx, box, m.Job()+"/ready.json", Ready{1, m.RunID, m.PublicSHA, "caller-1", m.CallerID, m.Attempt, ids, msg.PID, time.Now(), probes, host}) != Committed {
					streamErr = errors.New("ready-publication-unknown")
				}
			case "stats":
				var envelope struct {
					Stats json.RawMessage `json:"stats"`
				}
				_ = decode(line, &envelope)
				rawLine := bytes.Clone(line)
				streamErr = RecordGeneratorSample(envelope.Stats, &w, func() error {
					key := fmt.Sprintf("%s/stats/%05d.json", m.Job(), stats)
					if stats >= maxSafeInteger {
						return errors.New("caller-stats-exhausted")
					}
					stats++
					if box.Create(ctx, key, rawLine) != Committed {
						return errors.New("stats-publication-unknown")
					}
					return nil
				}, stop, interrupt)
			case "done":
				done = true
			}
			if streamErr != nil {
				break
			}
		}
		if streamErr == nil {
			streamErr = scanner.Err()
		}
		if streamErr != nil {
			close(streamFailed)
			cancelDelivery()
			interrupt()
			_ = stop("Caller stream/evidence failure")
			_, _ = io.Copy(io.Discard, reader)
		}
		stream <- streamErr
	}()
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go func() {
		for {
			select {
			case <-watchCtx.Done():
				interrupt()
				return
			case <-exited:
				return
			default:
			}
			hung, e := Stopped(watchCtx, box, m.Identity)
			if e == nil && hung {
				close(hungUp)
				cancelDelivery()
				interrupt()
				return
			}
			if sleep(watchCtx, 200*time.Millisecond) != nil {
				return
			}
		}
	}()
	sequence := 0
	running := true
	for running {
		select {
		case <-exited:
			running = false
			continue
		case <-ctx.Done():
			err = ctx.Err()
			running = false
			continue
		case <-streamFailed:
			err = errors.New("caller-stream-failed")
			running = false
			continue
		case <-hungUp:
			err = errors.New("conductor-stopped")
			running = false
			continue
		default:
		}
		stateMu.Lock()
		ready := httpURL != ""
		ids := append([]string{}, sessions...)
		stateMu.Unlock()
		if ready {
			var r ReadResult
			r, err = box.Read(ctx, sequenceKey(m.Root(), sequence))
			if err != nil {
				break
			}
			if r.Found {
				var feed Feed
				if decode(r.Data, &feed) != nil || feed.SchemaVersion != 1 || feed.RunID != m.RunID || feed.Sequence != sequence || !operationPattern.MatchString(feed.OperationID) {
					err = errors.New("invalid-caller-feed")
					break
				}
				ack, e := Deliver(deliveryCtx, feed, ids, local, interrupt, SystemClock{})
				if e != nil {
					ack.Error = "feed-rejected"
				}
				if createJSON(ctx, box, ackKey(m.Job(), sequence), ack) != Committed {
					err = errors.New("ack-publication-unknown")
					break
				}
				if sequence >= maxSafeInteger {
					err = errors.New("caller-sequence-exhausted")
					break
				}
				sequence++
				if e != nil {
					err = e
					break
				}
			}
		}
		if err = sleep(ctx, 100*time.Millisecond); err != nil {
			break
		}
	}
	if err != nil {
		interrupt()
		_ = stop("Caller supervision failure")
	}
	<-exited
	streamErr := <-stream
	_ = reader.Close()
	finalCtx, finalCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer finalCancel()
	data, readErr := os.ReadFile(report)
	if readErr == nil && len(data) > operator.MaxBlobBytes {
		readErr = errors.New("caller-report-size")
	}
	if readErr == nil {
		if box.Create(finalCtx, m.Job()+"/caller.jsonl", data) != Committed {
			readErr = errors.New("report-publication-unknown")
		}
	}
	stateMu.Lock()
	wasStopped := stopped
	ids := append([]string{}, sessions...)
	stateMu.Unlock()
	exitCode := child.ProcessState.ExitCode()
	finished := map[string]any{"runId": m.RunID, "job": "caller-1", "done": done, "pid": child.Process.Pid, "exitCode": exitCode, "stats": stats, "sessions": ids, "stopped": wasStopped}
	if createJSON(finalCtx, box, m.Job()+"/finished.json", finished) != Committed {
		return errors.New("finished-publication-unknown")
	}
	if !done {
		return errors.New("caller-evidence-incomplete")
	}
	return errors.Join(err, exitErr, streamErr, readErr)
}
func localWithBase(ctx context.Context, base string, client *http.Client, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	r, err := client.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return errors.New("caller-local-rejected")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, operator.MaxFrameBytes+1))
	if err != nil {
		return err
	}
	if len(data) > operator.MaxFrameBytes {
		return errors.New("caller-local-size")
	}
	return decode(data, out)
}
