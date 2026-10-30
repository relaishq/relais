package controlplane

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

type stalledHandshakeTransport struct {
	body     io.ReadCloser
	uploaded chan struct{}
}

func (s stalledHandshakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	go func() { _, _ = io.Copy(io.Discard, r.Body); close(s.uploaded) }()
	return &http.Response{StatusCode: http.StatusOK, ProtoMajor: 2, Header: make(http.Header), Body: s.body, Request: r}, nil
}

func TestDialOutHandshakeDeadlineInterruptsBodyRead(t *testing.T) {
	w, err := mediaworker.New(mediaworker.Config{ListenAddr: "127.0.0.1:0"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	reader, writer := io.Pipe()
	uploaded := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := false
	defer func() {
		cancel()
		_ = writer.Close()
		if !finished {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("handshake did not stop after closing its body")
			}
		}
		select {
		case <-uploaded:
		case <-time.After(3 * time.Second):
			t.Error("handshake upload did not stop")
		}
	}()
	d := &DialOut{Worker: w, URL: "http://control", Name: "worker", instance: "process"}
	go func() {
		done <- d.connect(ctx, &http.Client{Transport: stalledHandshakeTransport{body: reader, uploaded: uploaded}})
	}()
	select {
	case err := <-done:
		finished = true
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("handshake body read outlived its two-second command deadline")
	}
}
