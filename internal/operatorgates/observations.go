package operatorgates

import (
	"context"
	"encoding/json"
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
	err := s.observeHosts(ctx, p, "inventory", end, func(slot string) (string, error) {
		arch := "arm64"
		if len(pins) > 0 && pins[0].Architecture != "" {
			arch = pins[0].Architecture
		}
		return host.Inventory(slot, pins, requireRunning, arch)
	}, func(slot, stdout string) error {
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
	if deployedSHA != "" && slices.ContainsFunc(pool.Participants, func(p releasecompat.Participant) bool { return p.Commit != deployedSHA }) {
		return releasecompat.Pool{}, errors.New("deployed release differs from judge pin")
	}
	return pool, nil
}

func (s *Session) FreshClocks(ctx context.Context, p Peer, bounds clocks.Bounds, end time.Duration) (clocks.Evidence, error) {
	rows := map[string]clocks.Sample{}
	err := s.observeHosts(ctx, p, "clock", end, func(string) (string, error) { return host.Clock(), nil }, func(slot, stdout string) error {
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
	evidence, err := clocks.Check(rows, bounds)
	evidence.At = s.clock.Wall()
	return evidence, err
}

// The optional batch changes transport only. Every host still supplies fresh
// independent evidence, and no mutation can start before the whole batch joins.
func (s *Session) observeHosts(ctx context.Context, p Peer, action string, end time.Duration, script func(string) (string, error), decode func(string, string) error) (err error) {
	if !s.capabilities[operator.HostReadBatch] {
		for _, slot := range inventory.Slots {
			source, err := script(slot)
			if err != nil {
				return err
			}
			if err = s.observeHost(ctx, p, slot, action, source, end, func(stdout string) error { return decode(slot, stdout) }); err != nil {
				return err
			}
		}
		return nil
	}
	args := operator.HostReadBatchArgs{}
	for _, slot := range inventory.Slots {
		source, err := script(slot)
		if err != nil {
			return err
		}
		args.Reads = append(args.Reads, operator.HostRead{Target: slot, Script: []byte(source)})
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	diagnosed := false
	defer func() {
		if !diagnosed {
			s.observationFailure("hosts", action, err)
		}
	}()
	var observed error
	var childFailure *operator.Uncertainty
	outcome, err := s.Exchange(ctx, p, operator.HostReadBatch, "hosts", action, raw, nil, end, func(reply operator.Reply) operator.Certainty {
		if reply.NotSubmitted {
			return operator.Failed
		}
		var batch operator.HostReadBatchOutput
		if observe.Decode(reply.Payload, &batch, observe.Contract{"replies"}) != nil || len(batch.Replies) != len(inventory.Slots) {
			return operator.Unknown
		}
		unknown := func(slot string, child operator.Reply, rule string) operator.Certainty {
			if child.UncertaintyRule != "" {
				rule = child.UncertaintyRule
			}
			childFailure = &operator.Uncertainty{RequestID: reply.ID, Target: slot, Rule: rule}
			return operator.Unknown
		}
		outputs := make([]operator.HostOutput, len(batch.Replies))
		// Check every transport before decoding any observations. A definite failure
		// on one host must never hide uncertainty on another host.
		for i, child := range batch.Replies {
			if child.ID != inventory.Slots[i] || !child.Transport.Valid() || child.Blob != nil {
				return unknown(inventory.Slots[i], child, "malformed-reply")
			}
			if child.Transport == operator.Unknown {
				return unknown(inventory.Slots[i], child, "transport-unknown")
			}
			if child.NotSubmitted {
				if child.Transport != operator.Failed || !refusalCode(child.RefusalCode) {
					return unknown(inventory.Slots[i], child, "malformed-reply")
				}
				continue
			}
			if child.ExitCode == nil {
				return unknown(inventory.Slots[i], child, "missing-exit-status")
			}
			if observe.Decode(child.Payload, &outputs[i], observe.Contract{"stdout"}) != nil {
				return unknown(inventory.Slots[i], child, "malformed-reply")
			}
			if len(outputs[i].Stdout) > operator.MaxHostOutputBytes {
				return unknown(inventory.Slots[i], child, "truncated-output")
			}
		}
		for i, child := range batch.Replies {
			var e error
			if child.NotSubmitted {
				e = errors.New("precondition-failed:" + child.RefusalCode)
			} else {
				e = decode(inventory.Slots[i], outputs[i].Stdout)
			}
			if e == nil && (child.Transport != operator.Succeeded || *child.ExitCode != 0) {
				e = errors.New("precondition-failed")
			}
			if e != nil {
				s.observationFailure(inventory.Slots[i], action, e)
				diagnosed = true
				if observed == nil {
					observed = e
				}
			}
		}
		if observed != nil {
			return operator.Failed
		}
		return operator.Succeeded
	})
	if childFailure != nil {
		// Exchange has latched the unknown result before diagnostic callbacks.
		s.noteUnknownHost(childFailure.RequestID, childFailure.Target, action, childFailure.Rule)
		s.observationFailure(childFailure.Target, action, errors.New("command-result-unknown"))
		diagnosed = true
	}
	if err != nil {
		return err
	}
	if outcome == operator.Unknown {
		return errors.New("command-result-unknown")
	}
	if observed != nil {
		return observed
	}
	if outcome != operator.Succeeded {
		return errors.New("precondition-failed")
	}
	return nil
}
