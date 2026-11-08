package host

import (
	"errors"
	"slices"

	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
)

// LeaseReadScripts validates the closed wire contract at each receiving boundary.
// All executable bytes come from ClockWithLease; peer input is encoded JSON data.
func LeaseReadScripts(req operator.Request, probeFiles map[string]string) ([]operator.HostRead, error) {
	var args operator.HostLeaseReadArgs
	invalid := errors.New("invalid-lease-read")
	if len(req.Script) != 0 || req.Action != "clock" || observe.DecodeClosedLimit(req.Args, &args, observe.Contract{"reads"}, operator.MaxFrameBytes/2) != nil {
		return nil, invalid
	}
	slots := []string{req.Target}
	switch req.Operation {
	case operator.HostLeaseRead:
	case operator.HostLeaseReadBatch:
		slots = inventory.Slots
	default:
		return nil, invalid
	}
	if len(args.Reads) != len(slots) {
		return nil, invalid
	}
	reads := []operator.HostRead{}
	for i, input := range args.Reads {
		if input.Target != slots[i] || !slices.Contains(inventory.Slots, input.Target) {
			return nil, invalid
		}
		if input.ProbeFile != "" && input.ProbeFile != probeFiles[input.Target] {
			return nil, invalid
		}
		reads = append(reads, operator.HostRead{Target: input.Target, Script: []byte(ClockWithLease(input.Cursor, input.RunStartUnixMS, input.ProbeFile, input.ProbeCursor))})
	}
	return reads, nil
}
