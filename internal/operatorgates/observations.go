package operatorgates

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/internal/releasecompat"
	"github.com/relaishq/relais/pkg/operator"
)

// observeHost uses the session's single exchange path. Only the two fixed pure
// observations below call it. A malformed reply or unknown transport latches the
// session; a confirmed bad observation is a definite read precondition failure.
func (s *Session) SetObservationFailureSink(sink func(slot, observation string, err error)) {
	s.observationFailureSink = sink
}

func (s *Session) observationFailure(slot, action string, err error) {
	if err != nil && s.observationFailureSink != nil {
		s.observationFailureSink(slot, action, err)
	}
}

func (s *Session) observeHost(ctx context.Context, p Peer, slot, action, script string, end time.Duration, decode func(string) error) (err error) {
	defer func() { s.observationFailure(slot, action, err) }()
	var observationErr error
	outcome, err := s.Exchange(ctx, p, operator.HostExecute, slot, action, nil, []byte(script), end, func(reply operator.Reply) operator.Certainty {
		if reply.NotSubmitted {
			return operator.Failed
		}
		var output operator.HostOutput
		if err := observe.Decode(reply.Payload, &output, observe.Contract{"stdout"}); err != nil || len(output.Stdout) > operator.MaxHostOutputBytes {
			return operator.Unknown
		}
		observationErr = decode(output.Stdout)
		if reply.Transport != operator.Succeeded || reply.ExitCode == nil || *reply.ExitCode != 0 || observationErr != nil {
			return operator.Failed
		}
		return operator.Succeeded
	})
	if err != nil {
		return err
	}
	if outcome == operator.Unknown {
		return errors.New("command-result-unknown")
	}
	if observationErr != nil {
		return observationErr
	}
	if outcome != operator.Succeeded {
		return errors.New("precondition-failed")
	}
	return nil
}

func (s *Session) FreshInventory(ctx context.Context, p Peer, pins []inventory.Pin, end time.Duration, deployedSHA string, requireRunning bool) (releasecompat.Pool, error) {
	if err := inventory.ValidatePins(pins, s.scenario); err != nil {
		return releasecompat.Pool{}, err
	}
	pool := releasecompat.Pool{Schema: 1, Complete: true, TargetWorkerProfile: "legacy"}
	for _, slot := range inventory.Slots {
		arch := "arm64"
		if len(pins) > 0 && pins[0].Architecture != "" {
			arch = pins[0].Architecture
		}
		script, err := host.Inventory(slot, pins, requireRunning, arch)
		if err != nil {
			return releasecompat.Pool{}, err
		}
		err = s.observeHost(ctx, p, slot, "inventory", script, end, func(stdout string) error {
			data, err := host.Unpack(stdout, 256000)
			if err != nil {
				return inventory.Unavailable(slot, stdout)
			}
			rows, err := inventory.Parse(slot, data, pins)
			if err == nil {
				pool.Participants = append(pool.Participants, rows...)
			}
			return err
		})
		if err != nil {
			return releasecompat.Pool{}, err
		}
	}
	if deployedSHA != "" && slices.ContainsFunc(pool.Participants, func(p releasecompat.Participant) bool { return p.Commit != deployedSHA }) {
		return releasecompat.Pool{}, errors.New("deployed release differs from judge pin")
	}
	return pool, nil
}

func (s *Session) FreshClocks(ctx context.Context, p Peer, bounds clocks.Bounds, end time.Duration) (clocks.Evidence, error) {
	rows := map[string]clocks.Sample{}
	for _, slot := range inventory.Slots {
		err := s.observeHost(ctx, p, slot, "clock", host.Clock(), end, func(stdout string) error {
			data, err := host.Unpack(stdout, 16000)
			if err != nil {
				return err
			}
			rows[slot], err = clocks.Parse(data)
			return err
		})
		if err != nil {
			return clocks.Evidence{}, err
		}
	}
	evidence, err := clocks.Check(rows, bounds)
	evidence.At = s.clock.Wall()
	return evidence, err
}
