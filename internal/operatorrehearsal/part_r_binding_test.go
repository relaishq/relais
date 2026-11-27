package operatorrehearsal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/bindingtest"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/releasecompat"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

type partRBindingClock struct{ operatorgates.Clock }

// synctest starts in 2000; clock evidence requires 19-digit nanoseconds.
func (c partRBindingClock) Wall() time.Time { return c.Clock.Wall().AddDate(26, 0, 0) }

// Exercise R's actual rehearsal replacement and container adapter, including
// its batched inventory. The earlier five-host test used a serial fake peer.
func TestPartRBindAdmissionFiveHosts(t *testing.T) {
	for _, mode := range []string{"valid", "invalid-target", "manifest-identity"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixtures, pins := idleInventory(t)
				pins = pins[:1]
				pins[0].Version, pins[0].Commit, pins[0].ArchiveSHA256 = ReleaseRVersion, ReleaseRCommit, ReleaseRSHA256
				for slot, raw := range fixtures {
					var value struct {
						ObservedAt   int64                       `json:"observedAt"`
						Participants []releasecompat.Participant `json:"participants"`
					}
					require.NoError(t, json.Unmarshal(raw, &value))
					for i := range value.Participants {
						p := &value.Participants[i]
						p.Version, p.Commit, p.ArchiveSHA256 = ReleaseRVersion, ReleaseRCommit, ReleaseRSHA256
						p.Manifest.Version, p.Manifest.Commit = ReleaseRVersion, ReleaseRCommit
					}
					fixtures[slot], _ = json.Marshal(value)
					if slot == "relay" {
						value.Participants = value.Participants[:1]
						value.Participants[0].Name = "relay-two"
						fixtures["relay-two"], _ = json.Marshal(value)
					}
				}
				clock := partRBindingClock{operatorgates.NewRealClock()}
				now := clock.Wall()
				f, err := bindingtest.NewWithWindow("part-r-binding", now, rehearsalClockAdmissionWindowSeconds)
				require.NoError(t, err)
				fifth := f.Input.Hosts[0]
				fifth.Slot = "relay-two"
				f.Input.Hosts = append(f.Input.Hosts, fifth)
				f.Selection, err = clocks.Select(f.Input, f.Policy)
				require.NoError(t, err)
				require.Len(t, f.Selection.Receipt.Hosts, 5)
				dir := t.TempDir()
				for name, value := range map[string]any{"clock-selection.json": f.Selection, "clock-admission-evidence.json": f.Input} {
					raw, err := json.Marshal(value)
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(filepath.Join(dir, name), raw, 0600))
				}
				topology := partRTopology(t)
				containers := fixtureContainerIDs()
				containers["relay-two"] = strings.Repeat("e", 64)
				runner := commandRunner(func(_ context.Context, c Command) CommandResult {
					if c.LeaseCPU {
						time.Sleep(20 * time.Millisecond)
					}
					out := f.Wire(string(c.Input), clock.Wall())
					if !strings.Contains(string(c.Input), "read_clock(") && !strings.Contains(string(c.Input), "historical_tracking(") {
						for slot, id := range containers {
							if c.Args[3] == id {
								out = bindingtest.Pack(fixtures[slot])
							}
						}
					}
					return CommandResult{Code: code(0), Stdout: []byte(out)}
				})
				var diagnostics bytes.Buffer
				bounds := f.Selection.Receipt.Bounds
				r := &rehearsal{run: "part-r-binding", clock: clock, topology: topology, runner: runner, pins: pins, diagnostics: &diagnostics,
					c:          RehearseConfig{PartR: true, Commit: strings.Repeat("c", 40), Output: dir},
					report:     &Report{TemplateSHA256: strings.Repeat("a", 64), CallerSHA256: strings.Repeat("b", 64), ClockSelection: &ClockSelectionReport{Bounds: &bounds}},
					relayJudge: operatorevidence.OpenRelayLossJudge(strings.Repeat("c", 40), strings.Repeat("d", 64), "/judge"),
					judge:      &recordingJudge{pinned: operatorevidence.PinnedJudge{Binary: "/judge"}},
				}
				scenario, err := r.partRScenario()
				require.NoError(t, err)
				init := operator.Init{Scenario: scenario, ArmedAtUnixMS: fmt.Sprint(now.UnixMilli()), EndUnixMS: fmt.Sprint(now.Add(time.Hour).UnixMilli())}
				r.boundInit = init
				r.session, err = operatorgates.NewSession(r.run, init, clock)
				require.NoError(t, err)
				if mode == "manifest-identity" {
					var value map[string]any
					require.NoError(t, json.Unmarshal(fixtures["relay-two"], &value))
					value["participants"].([]any)[0].(map[string]any)["manifest"].(map[string]any)["commit"] = strings.Repeat("f", 40)
					fixtures["relay-two"], _ = json.Marshal(value)
				}
				if mode == "invalid-target" {
					delete(containers, "relay-two")
				}
				old := r.session
				r.peer = NewLocalPeer(&Adapter{Topology: topology, Containers: containers, Runner: runner}, r.run, init)
				defer func() { _ = r.peer.Close() }()
				r.deployment = &Deployment{Topology: topology, containerIDs: containers}
				r.runtime = &operatorgates.ProcessRuntime{Session: old, Peer: r.peer}
				err = r.bindAdmission(t.Context(), scenario)
				if mode != "valid" {
					require.Error(t, err)
					require.Same(t, old, r.session)
					require.False(t, r.session.Latched())
					if mode == "invalid-target" {
						require.EqualError(t, err, "precondition-failed:invalid-target")
						require.Contains(t, diagnostics.String(), "observation=inventory category=precondition-failed:invalid-target")
					} else {
						require.EqualError(t, err, "inventory-unavailable:relay-two-manifest-identity")
						require.Contains(t, diagnostics.String(), "host=relay-two observation=inventory category=precondition-failed:relay-two-manifest-identity")
					}
					return
				}
				require.NoError(t, err, diagnostics.String())
				require.NotSame(t, old, r.session)
				require.NotNil(t, r.runtime.Leases)
				require.Equal(t, init.ArmedAtUnixMS, r.boundInit.ArmedAtUnixMS)
				require.Equal(t, init.EndUnixMS, r.boundInit.EndUnixMS)
				require.Equal(t, scenario.Judge, r.boundInit.Scenario.Judge)
				require.Equal(t, f.Selection.SHA256, r.boundInit.Scenario.ClockReceiptSHA256)
				// The next resource admission uses the same five-host adapter for lease reads.
				require.NoError(t, r.runtime.Leases.Refresh(t.Context(), time.Minute))
				snapshot, err := r.runtime.Leases.Snapshot()
				require.NoError(t, err)
				require.Len(t, snapshot.Clocks, 5)
			})
		})
	}
}
