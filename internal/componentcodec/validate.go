package componentcodec

import (
	"fmt"
	"net/netip"
)

// Validate checks canonical component grammar, bound session identity, live
// host versions and DTLS counters. Mediaworker assembly also validates the
// application registration and Pion-specific restore invariants before use.
func Validate(id string, c Components) error {
	var setup ConnectionSetup
	var transport TransportState
	var work, history []byte
	for _, part := range []struct {
		kind byte
		data []byte
		out  any
	}{{Setup, c.Setup, &setup}, {Transport, c.Transport, &transport}, {Work, c.Work, &work}, {History, c.History, &history}} {
		if err := Unmarshal(part.data, part.kind, part.out); err != nil {
			return err
		}
	}
	if setup.ID != id || setup.LocalUfrag != id || id == "" {
		return fmt.Errorf("%w: session identity", ErrInvalid)
	}
	if transport.LegacyVersion < 7 || transport.LegacyVersion > 9 {
		return fmt.Errorf("%w: host version", ErrInvalid)
	}
	if transport.DTLS.LocalEpoch == 0 || transport.DTLS.SequenceNumber > 1<<48 {
		return fmt.Errorf("%w: DTLS progress", ErrInvalid)
	}
	address, err := netip.ParseAddrPort(transport.RemoteAddr)
	if err != nil || address.Addr().Is4In6() || address.String() != transport.RemoteAddr {
		return fmt.Errorf("%w: address", ErrInvalid)
	}
	if len(transport.Host) > 2048 {
		return fmt.Errorf("%w: host metadata size", ErrInvalid)
	}
	return nil
}
