package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/mediaworker"
)

func TestUncertainProxyRetainsUpstreamConnectionAfterDiscard(t *testing.T) {
	var connections atomic.Int32
	worker := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request mediaworker.ResumeRequest
		if !privateapi.Read(w, r, &request) {
			return
		}
		privateapi.Write(w, mediaworker.ResumeReply{ID: "legacy-echo-session"})
	}))
	worker.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	worker.Start()
	defer worker.Close()
	proxy, err := newUncertainProxy("127.0.0.1", worker.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	request := mediaworker.ResumeRequest{State: []byte("echo")}
	var reply mediaworker.ResumeReply
	proxy.Arm()
	err = privateapi.Do(context.Background(), client, proxy.URL, http.MethodPost, "/resume", request, &reply, mediaworker.RemoteErrors)
	if !errors.Is(err, privateapi.ErrUncertain) || proxy.Count() != 1 {
		t.Fatalf("fault missing: err=%v count=%d", err, proxy.Count())
	}
	before := connections.Load()
	err = privateapi.Do(context.Background(), client, proxy.URL, http.MethodPost, "/resume", request, &reply, mediaworker.RemoteErrors)
	if err != nil || reply.ID != "legacy-echo-session" {
		t.Fatalf("second resume: reply=%+v err=%v", reply, err)
	}
	if after := connections.Load(); after != before {
		t.Fatalf("discarded reply destroyed upstream TCP connection: before=%d after=%d", before, after)
	}
}
