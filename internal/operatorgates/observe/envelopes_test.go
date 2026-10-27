package observe

import (
	"testing"

	"github.com/relaishq/relais/pkg/relay"
)

func TestRelayPIDPresence(t *testing.T) {
	for _, c := range []struct {
		name    string
		standby string
		valid   bool
		pid     int
	}{
		{"missing", "", false, 0},
		{"null", `,"relais-relay-standby.service":null`, false, 0},
		{"negative", `,"relais-relay-standby.service":-1`, false, 0},
		{"stopped", `,"relais-relay-standby.service":0`, true, 0},
		{"running", `,"relais-relay-standby.service":456`, true, 456},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := []byte(`{"active":"relais-relay.service","pids":{"relais-relay.service":123` + c.standby + `},"status":` + string(marshal(t, relay.RelayStatus{})) + `}`)
			got, err := RelayInspection(data)
			if (err == nil) != c.valid {
				t.Fatalf("standby PID evidence: %v", err)
			}
			if c.valid && (got.PIDs[got.Active] != 123 || got.PIDs["relais-relay-standby.service"] != c.pid) {
				t.Fatal("valid PID evidence changed")
			}
		})
	}
}
