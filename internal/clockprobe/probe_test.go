package clockprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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
