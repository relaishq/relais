package sessionstore

import (
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/redistestcluster"
	"github.com/stretchr/testify/require"
)

// The table is the slot audit for every process-store script and multi-key
// operation. Keys come from the same builders and slices as the production call.
// Pipelines batch independent commands and do not require a common slot.
func TestRedisScriptSlotSafety(t *testing.T) {
	redistestcluster.AuditScripts(t, "sessionLua", "indexLua", "indexRefreshLua", "relayLeaseLua", "putRouteLua", "deleteRouteLua", "storeClockLua", "sessionScript.Hash()", "componentCommitLua", "componentReadLua", "controlLeaseLua", "controlIntentLua")
	r := &Redis{prefix: "slot-test:"}
	for _, id := range []string{"call", "{untrusted}", "~encoded", "", "a}b{c"} {
		keys := r.lifecycleKeys(id)
		operations := []struct {
			name, lua string
			keys      []string
		}{
			{"session transitions and recovery", sessionLua, keys},
			{"control authority and records", controlLeaseLua, []string{r.controlKey()}},
			{"control session intent", controlIntentLua, []string{r.intentKey(id)}},
			{"worker-index fenced EVALSHA check", sessionLua, keys},
			{"route-restore owner read", sessionLua, keys},
			{"sequence reservation (lease-only branch)", "", r.sessionKeys(id, "sequence")},
			{"legacy hot put", sessionLua, r.sessionKeys(id, "put")},
			{"component commit", componentCommitLua, r.componentKeys(id)},
			{"component tuple read", componentReadLua, r.componentKeys(id)},
			{"store clock", storeClockLua, r.sessionKeys(id, "clock")},
			{"route publish", putRouteLua, []string{keys[0], keys[4]}},
			{"route compare-delete and scan cleanup", deleteRouteLua, []string{keys[4]}},
			{"route forget DEL", "", []string{keys[4]}},
			{"worker-index publish", indexLua, []string{r.indexKey(netip.MustParseAddrPort("127.0.0.1:12345"))}},
			{"worker-index refresh", indexRefreshLua, []string{r.indexKey(netip.MustParseAddrPort("127.0.0.1:12345"))}},
			{"relay tenure transitions", relayLeaseLua, []string{r.relayKey(id)}},
		}
		for _, operation := range operations {
			t.Run(id+"/"+operation.name, func(t *testing.T) {
				redistestcluster.SameSlot(t, operation.keys...)
				script := operation.lua
				if operation.name == "legacy hot put" {
					// The real script dispatches many operations. Only put's branch
					// executes on its two-key hot path; its common lease read uses KEYS[1].
					var found bool
					_, script, found = strings.Cut(script, "elseif op=='put' then")
					require.True(t, found, "audit must include the production put branch")
				}
				for _, match := range regexp.MustCompile(`KEYS\[\s*(\d+)\s*\]`).FindAllStringSubmatch(script, -1) {
					index, err := strconv.Atoi(match[1])
					require.NoError(t, err)
					require.LessOrEqual(t, index, len(operation.keys), "unaccounted script key")
				}
			})
		}
	}
}
