package framecache

import (
	"regexp"
	"testing"

	"github.com/relaishq/relais/internal/redistestcluster"
	"github.com/stretchr/testify/require"
)

func TestRedisFrameScriptSlotSafety(t *testing.T) {
	redistestcluster.AuditScripts(t, "appendLua", "currentLua")
	r := &Redis{prefix: "slot-test:"}
	for _, id := range []string{"call", "{untrusted}", "~encoded"} {
		for _, script := range []string{appendLua, currentLua} {
			redistestcluster.SameSlot(t, r.key(id))
			for _, ref := range regexp.MustCompile(`KEYS\[([^]]+)\]`).FindAllStringSubmatch(script, -1) {
				require.Equal(t, "1", ref[1], "frame operations must remain single-key")
			}
		}
	}
}
