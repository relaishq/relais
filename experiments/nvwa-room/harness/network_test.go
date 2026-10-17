package harness

import (
	"github.com/relaishq/relais/internal/nettopology"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestNetworkRoutesShareCallerLinkAndRestrictServices(t *testing.T) {
	p, err := nettopology.NewPlan(1000, 123, "0102030405", 2)
	require.NoError(t, err)
	c := Config{RoomURL: "http://" + p.Roles["store"].Private.String() + ":33001", SignalURL: "https://" + p.Roles["relay"].Private.String() + ":33002", Media: p.Roles["relay"].Public.String() + ":33003"}
	commands := NetworkCommands(p, c)
	var output []string
	for _, cmd := range commands {
		output = append(output, strings.Join(cmd, " "))
	}
	text := strings.Join(output, "\n")
	require.Contains(t, text, "route add "+p.Datacentre.String()+" via "+p.Roles["relay"].Public.String())
	require.Contains(t, text, "-p tcp -d "+p.Roles["store"].Private.String()+" --dport 33001 -j ACCEPT")
	require.Contains(t, text, "-p udp -d "+p.Roles["relay"].Public.String()+" --dport 33003 -j ACCEPT")
	require.NotContains(t, text, p.ManagementHost)
	require.NotContains(t, text, " -P ")    // no global ACCEPT policy
	require.NotContains(t, text, "rto_min") // no special TCP recovery tuning
}
