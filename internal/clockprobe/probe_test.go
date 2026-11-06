package clockprobe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.GotConn != nil {
		trace.GotConn(httptrace.GotConnInfo{})
	}
	return f(r)
}

// No ready-connection trace: model a failure during dialing instead of sending.
type failedDialTransport struct{}

func (failedDialTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, &url.Error{Op: "Get", URL: r.URL.String(), Err: errors.New("unreachable")}
}

type countingBody struct {
	reader io.Reader
	read   int
	closed bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}

func (b *countingBody) Close() error { b.closed = true; return nil }

func testTargets(t *testing.T) Targets {
	t.Helper()
	var targets Targets
	for i, name := range []string{"control", "w1", "w2"} {
		require.NoError(t, targets.Set(name+"=http://127.0.0.1:"+strconv.Itoa(i+1)))
	}
	return targets
}

func TestInProcessOffsetHandlerRecordsFourTimestampsAtOneHz(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probes")
	log, err := OpenLog(path)
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/clock" {
				return nil, errors.New("wrong path")
			}
			response := httptest.NewRecorder()
			Handler(func() time.Time { return time.Now().Add(10 * time.Millisecond) }).ServeHTTP(response, r)
			return response.Result(), nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- run(ctx, testTargets(t), log.Append, probeClient(transport)) }()
		synctest.Wait()
		time.Sleep(2100 * time.Millisecond)
		cancel()
		require.NoError(t, <-done)
	})
	require.NoError(t, log.Close())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Len(t, data, 9*LineBytes)
	byTarget := map[Target][]int64{}
	for _, line := range bytes.SplitAfter(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		r, err := ParseLine(line)
		require.NoError(t, err)
		require.Equal(t, Success, r.Result)
		// An offset larger than the network interval is retained, never clamped.
		require.Equal(t, int64(10*time.Millisecond), r.HostSend-r.FrontendReceive)
		require.Equal(t, int64(10*time.Millisecond), r.HostReceive-r.FrontendSend)
		byTarget[r.Target] = append(byTarget[r.Target], r.FrontendSend)
	}
	for _, target := range []Target{Control, WorkerOne, WorkerTwo} {
		times := byTarget[target]
		require.Len(t, times, 3)
		require.Equal(t, int64(time.Second), times[1]-times[0])
		require.Equal(t, int64(time.Second), times[2]-times[1])
	}
}

func TestSamplerTimeoutsDoNotOverlapAndCancellationJoinsEveryTarget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		active, peak := map[string]int{}, map[string]int{}
		var records []Record
		transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			active[r.URL.Host]++
			peak[r.URL.Host] = max(peak[r.URL.Host], active[r.URL.Host])
			mu.Unlock()
			<-r.Context().Done()
			mu.Lock()
			active[r.URL.Host]--
			mu.Unlock()
			return nil, r.Context().Err()
		})
		appendRecord := func(r Record) error {
			mu.Lock()
			defer mu.Unlock()
			records = append(records, r)
			return nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- run(ctx, testTargets(t), appendRecord, probeClient(transport)) }()
		synctest.Wait()
		time.Sleep(2500 * time.Millisecond)
		cancel()
		require.NoError(t, <-done)
		require.Len(t, records, 9)
		for _, r := range records {
			require.Equal(t, RequestFailed, r.Result)
			duration := r.FrontendReceive - r.FrontendSend
			require.True(t, duration == int64(probeTimeout) || duration == int64(500*time.Millisecond), "duration %d", duration)
		}
		require.Len(t, peak, 3)
		for host, n := range peak {
			require.Equal(t, 1, n, host)
			require.Zero(t, active[host], host)
		}
	})
}

func TestSamplerLogFailureCancelsAndJoinsOtherRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		failure := errors.New("log unavailable")
		transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Port() != "1" {
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
		})
		err := run(context.Background(), testTargets(t), func(Record) error { return failure }, probeClient(transport))
		require.ErrorIs(t, err, failure)
	})
}

func TestSamplerSerializesSlowRequestsAndAppends(t *testing.T) {
	for _, slowAppend := range []bool{false, true} {
		name := "slow-request"
		if slowAppend {
			name = "slow-append"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var mu sync.Mutex
				active, peak, calls := map[Target]int{}, map[Target]int{}, map[Target]int{}
				slow := func(target Target) {
					mu.Lock()
					active[target]++
					peak[target] = max(peak[target], active[target])
					calls[target]++
					mu.Unlock()
					// Deliberately outlast both the 750 ms timeout and 1 s tick.
					// The transport case ignores cancellation, like blocked I/O.
					time.Sleep(1500 * time.Millisecond)
					mu.Lock()
					active[target]--
					mu.Unlock()
				}
				transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if !slowAppend {
						i, _ := strconv.Atoi(r.URL.Port())
						slow(Target(i - 1))
					}
					return &http.Response{StatusCode: 503, Header: make(http.Header), Body: http.NoBody}, nil
				})
				appendRecord := func(r Record) error {
					if slowAppend {
						slow(r.Target)
					}
					return nil
				}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- run(ctx, testTargets(t), appendRecord, probeClient(transport)) }()
				synctest.Wait()
				time.Sleep(3500 * time.Millisecond)
				cancel()
				require.NoError(t, <-done)
				for _, target := range []Target{Control, WorkerOne, WorkerTwo} {
					require.GreaterOrEqual(t, calls[target], 2)
					require.Equal(t, 1, peak[target], "overlap on target %s", target)
					require.Zero(t, active[target], "unjoined target %s", target)
				}
			})
		})
	}
}

func TestProbeSendStartsAfterConnectionSetup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		setup := 200 * time.Millisecond
		serverDone := make(chan error, 1)
		transport := &http.Transport{DisableKeepAlives: true, DialContext: func(context.Context, string, string) (net.Conn, error) {
			time.Sleep(setup)
			client, server := net.Pipe()
			go func() {
				defer func() { _ = server.Close() }()
				req, err := http.ReadRequest(bufio.NewReader(server))
				if err != nil {
					serverDone <- err
					return
				}
				defer func() { _ = req.Body.Close() }()
				time.Sleep(time.Millisecond)
				rw := httptest.NewRecorder()
				Handler(time.Now).ServeHTTP(rw, req)
				response := rw.Result()
				defer func() { _ = response.Body.Close() }()
				time.Sleep(time.Millisecond)
				serverDone <- response.Write(server)
			}()
			return client, nil
		}}
		defer transport.CloseIdleConnections()
		start := time.Now()
		r := probe(context.Background(), probeClient(transport), Peer{Control, "http://127.0.0.1"})
		require.NoError(t, <-serverDone)
		require.Equal(t, Success, r.Result)
		require.Equal(t, start.Add(setup).UnixNano(), r.FrontendSend)
		require.Equal(t, int64(2*time.Millisecond), r.FrontendReceive-r.FrontendSend)
		require.Equal(t, setup+2*time.Millisecond, time.Since(start))
	})
}

func TestSamplerFullLogStopsEveryTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "full")
	marker, err := (Record{Target: capacityTarget, Result: CapacityReached}).MarshalLine()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, marker, 0600))
	log, err := OpenLog(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, log.Close()) }()
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		requests := 0
		client := probeClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
			mu.Lock()
			requests++
			mu.Unlock()
			return &http.Response{StatusCode: 503, Header: make(http.Header), Body: http.NoBody}, nil
		}))
		err := run(context.Background(), testTargets(t), log.Append, client)
		require.ErrorIs(t, err, ErrFull)
		require.GreaterOrEqual(t, requests, 1)
		require.LessOrEqual(t, requests, 3)
		finished := requests
		time.Sleep(10 * time.Second)
		require.Equal(t, finished, requests)
	})
}

func TestDialURLFailuresStayInRecords(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var mu sync.Mutex
		var records []Record
		err := run(ctx, testTargets(t), func(r Record) error {
			mu.Lock()
			records = append(records, r)
			mu.Unlock()
			cancel()
			return nil
		}, probeClient(failedDialTransport{}))
		require.NoError(t, err, "URL errors must not escape to the signaling warning")
		require.NotEmpty(t, records)
		for _, r := range records {
			require.Equal(t, RequestFailed, r.Result)
			require.Zero(t, r.FrontendSend, "no connection was ready")
		}
	})
}

func TestProbeRecordsFailuresAndInvalidResponsesWithoutRetry(t *testing.T) {
	now := time.Now().UTC()
	good, err := json.Marshal(Sample{ReceivedAt: now, SentAt: now})
	require.NoError(t, err)
	reversed, err := json.Marshal(Sample{ReceivedAt: now, SentAt: now.Add(-time.Second)})
	require.NoError(t, err)
	tooLong, err := json.Marshal(Sample{ReceivedAt: now, SentAt: now.Add(time.Hour)})
	require.NoError(t, err)
	outsideNS, err := json.Marshal(Sample{ReceivedAt: time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC), SentAt: now})
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		status int
		body   string
		result Result
	}{
		{"success", 200, string(good), Success},
		{"http-error", 503, string(good), RequestFailed},
		{"redirect", 302, string(good), RequestFailed},
		{"malformed-json", 200, "not JSON", InvalidSample},
		{"missing-timestamps", 200, "{}", InvalidSample},
		{"invalid-time", 200, `{"received_at":"bad","sent_at":"bad"}`, InvalidSample},
		{"inverted-host-time", 200, string(reversed), InvalidSample},
		{"negative-rtt", 200, string(tooLong), InvalidSample},
		{"timestamp-overflow", 200, string(outsideNS), InvalidSample},
		{"oversized-body", 200, string(good) + strings.Repeat(" ", maxBodyBytes), InvalidSample},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests++
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Location": {"http://127.0.0.1/other"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			r := probe(context.Background(), probeClient(transport), Peer{Control, "http://127.0.0.1"})
			require.Equal(t, tc.result, r.Result)
			require.Equal(t, 1, requests)
			require.GreaterOrEqual(t, r.FrontendReceive, r.FrontendSend)
			line, err := r.MarshalLine()
			require.NoError(t, err)
			_, err = ParseLine(line)
			require.NoError(t, err)
		})
	}
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unreachable") })
	r := probe(context.Background(), probeClient(transport), Peer{WorkerOne, "http://127.0.0.1"})
	require.Equal(t, RequestFailed, r.Result)
	require.Zero(t, r.HostReceive)
	require.Zero(t, r.HostSend)
}

func TestClockHandlerUsesUTCAndExactSampleShape(t *testing.T) {
	start := time.Date(2026, 11, 6, 1, 2, 3, 4, time.FixedZone("test", 3600))
	calls := 0
	response := httptest.NewRecorder()
	Handler(func() time.Time {
		at := start.Add(time.Duration(calls))
		calls++
		return at
	}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/clock", nil))
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	require.Equal(t, "application/json", response.Header().Get("Content-Type"))
	var fields map[string]string
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &fields))
	require.Equal(t, map[string]string{"received_at": start.UTC().Format(time.RFC3339Nano), "sent_at": start.Add(time.Nanosecond).UTC().Format(time.RFC3339Nano)}, fields)
}

func TestProbeBoundsBodyReadsAndClosesResponse(t *testing.T) {
	body := &countingBody{reader: strings.NewReader(strings.Repeat("x", 1<<20))}
	client := probeClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
	}))
	r := probe(context.Background(), client, Peer{Control, "http://127.0.0.1"})
	require.Equal(t, InvalidSample, r.Result)
	require.Equal(t, maxBodyBytes+1, body.read)
	require.True(t, body.closed)
}
