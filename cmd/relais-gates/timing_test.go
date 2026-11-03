package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/pkg/operator"
)

type timingFailureWriter struct{}

func (timingFailureWriter) Write([]byte) (int, error) { return 0, errors.New("write-failed") }

type completedTimingPeer struct {
	init  operator.Init
	final []byte
}

func (p *completedTimingPeer) Send(context.Context, operator.Frame) error { return nil }
func (p *completedTimingPeer) Receive(context.Context) (operator.Frame, error) {
	return operator.Frame{Type: "init", Session: "timing-fixture", Init: &p.init}, nil
}
func (p *completedTimingPeer) Poll() (operator.Frame, error, bool) {
	return operator.Frame{}, nil, false
}
func (p *completedTimingPeer) Close() error { return nil }
func (p *completedTimingPeer) Finish(_ context.Context, build func(error) operator.Frame) error {
	var err error
	p.final, err = json.Marshal(build(nil))
	return err
}

func TestTimingEvidenceWriteFailureKeepsSessionExitAndResult(t *testing.T) {
	fixture := sessionFixture(t)
	now := time.Now()
	init := operator.Init{Scenario: fixture.Scenario, ArmedAtUnixMS: strconv.FormatInt(now.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(now.Add(105*time.Minute).UnixMilli(), 10)}
	var control []byte
	for _, failed := range []bool{false, true} {
		writer := io.Discard
		if failed {
			writer = timingFailureWriter{}
		}
		timing := &operatorgates.TimingRecorder{Writer: writer}
		peer := &completedTimingPeer{init: init}
		err := operatorgates.ServeSequence(context.Background(), peer, operator.Hello{}, func(context.Context, *operatorgates.Session, operatorgates.Peer) []operator.GateResult {
			timing.RecordPool(operatorevidence.Pool{})
			timing.Finish()
			var gates []operator.GateResult
			for _, gate := range init.Scenario.Gates {
				gates = append(gates, operator.GateResult{Gate: gate, Status: operator.Passed})
			}
			return gates
		})
		var final operator.Frame
		if decodeErr := json.Unmarshal(peer.final, &final); decodeErr != nil || final.Final == nil || !final.Final.Complete || final.Final.Status != operator.Passed {
			t.Fatal("fixture did not complete a passing session", decodeErr, string(peer.final))
		}
		var stderr bytes.Buffer
		code := sessionExitCode(err, timing.Err(), &stderr)
		if err != nil || code != 0 {
			t.Fatal("diagnostic failure changed completed session exit", code, err)
		}
		if !failed {
			control = peer.final
			if stderr.Len() != 0 {
				t.Fatal("successful diagnostic output reported an error")
			}
		} else if !bytes.Equal(control, peer.final) || stderr.String() != "operator-timing-evidence-incomplete\n" {
			t.Fatal("diagnostic failure changed the final result or marker", string(peer.final), stderr.String())
		}
	}
	if code := sessionExitCode(errors.New("session-failed"), errors.New("write-failed"), io.Discard); code != 1 {
		t.Fatal("diagnostic handling erased a session failure", code)
	}
}
