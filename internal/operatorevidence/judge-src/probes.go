// Shared immutable registration to caller-report clock-prefix join.
package main

import (
	"time"

	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/callharness"
)

type registrationProbe struct {
	RTT           int64     `json:"rttNs"`
	Send          time.Time `json:"callerSend"`
	Receive       time.Time `json:"callerReceive"`
	ServerReceive time.Time `json:"serverReceive"`
	ServerSend    time.Time `json:"serverSend"`
}

func matchesProbePrefix(probes []registrationProbe, report *callharness.Report) bool {
	if observe.Numbers(probes) != nil || report == nil || report.Distributed == nil || len(report.Distributed.Probes) < len(probes) {
		return false
	}
	for i, expected := range probes {
		actual := report.Distributed.Probes[i]
		if expected.RTT <= 0 || expected.RTT > int64(time.Second) || actual.CallerReceive.Before(actual.CallerSend) || actual.CallerReceive.Sub(actual.CallerSend) > time.Second {
			return false
		}
		if !expected.Send.Equal(actual.CallerSend) || !expected.Receive.Equal(actual.CallerReceive) ||
			!expected.ServerReceive.Equal(actual.ServerReceive) || !expected.ServerSend.Equal(actual.ServerSend) ||
			expected.RTT != actual.CallerReceive.Sub(actual.CallerSend).Nanoseconds() {
			return false
		}
	}
	return true
}
