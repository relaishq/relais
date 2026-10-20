package mediaworker

import (
	"context"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
)

func (s *session) measuredSave(ctx context.Context, app agent.Agent) (agent.State, error) {
	finishCPU := beginSaveCPU()
	started := time.Now()
	state, err := app.Save(ctx)
	elapsed := time.Since(started)
	cpu, known := finishCPU()
	m := &s.worker.metrics
	m.saveCalls.Add(1)
	m.saveWallNS.Add(uint64(elapsed))
	if known {
		m.saveCPUNS.Add(cpu)
	} else {
		m.saveCPUUnknown.Add(1)
	}
	return state, err
}

// Application Save and host admission are distinct costs. Timers and thread
// clock reads add measurement overhead to both matched arms.
func (s *session) measureAdmission() func() {
	finishCPU := beginSaveCPU()
	started := time.Now()
	return func() {
		wall := time.Since(started)
		cpu, known := finishCPU()
		m := &s.worker.metrics
		m.admissionCalls.Add(1)
		m.admissionWallNS.Add(uint64(wall))
		if known {
			m.admissionCPUNS.Add(cpu)
		} else {
			m.admissionCPUUnknown.Add(1)
		}
	}
}
