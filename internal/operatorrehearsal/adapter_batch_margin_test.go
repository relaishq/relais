package operatorrehearsal

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

func TestPartRFiveHostReplyMargin(t *testing.T) {
	var worstPacked operator.HostReadBatchOutput
	for _, slot := range operator.HostSlots(true) {
		payload, err := json.Marshal(operator.HostOutput{Stdout: strings.Repeat("A", operator.MaxHostOutputBytes-1) + "\n"})
		require.NoError(t, err)
		worstPacked.Replies = append(worstPacked.Replies, operator.Reply{ID: slot, Transport: operator.Succeeded, ExitCode: code(0), Payload: payload})
	}
	packed, err := json.Marshal(worstPacked)
	require.NoError(t, err)
	for _, mode := range []string{"packed", "twice-packed", "exact-limit", "decoder-oversized", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			output := strings.Repeat("A", operator.MaxHostOutputBytes-1) + "\n"
			if mode == "twice-packed" {
				// JSON escaping expands these same-length host outputs beyond
				// twice the largest five-host base64 envelope, including metadata.
				output = strings.Repeat(`"`, operator.MaxHostOutputBytes-256) + strings.Repeat("<", 256)
			}
			if mode == "oversized" {
				output = strings.Repeat("\x00", 20000)
			}
			if mode == "decoder-oversized" || mode == "exact-limit" {
				// 257002 bytes: fits the former adapter limit (258048), but
				// cannot be read by the public strict decoder (256000).
				output = strings.Repeat(`"`, operator.MaxHostOutputBytes-831) + strings.Repeat("<", 831)
			}
			ids := fixtureContainerIDs()
			ids["relay-two"] = strings.Repeat("e", 64)
			adapter := &Adapter{Topology: partRTopology(t), Containers: ids, Runner: &fakeRunner{result: CommandResult{Code: code(0), Stdout: []byte(output)}}}
			if mode == "exact-limit" {
				adapter.Runner = leaseRunner(func(_ context.Context, c Command) CommandResult {
					stdout := output
					if c.Args[3] == ids["relay"] {
						// Replace 1002 escaped quotes with ordinary ASCII. This
						// removes exactly 1002 encoded bytes from the envelope.
						stdout = strings.Repeat("A", 1002) + output[1002:]
					}
					return CommandResult{Code: code(0), Stdout: []byte(stdout)}
				})
			}
			req := batchRequest()
			var args operator.HostReadBatchArgs
			require.NoError(t, json.Unmarshal(req.Args, &args))
			args.Reads = append(args.Reads, operator.HostRead{Target: "relay-two", Script: []byte("printf observation")})
			req.Args, err = json.Marshal(args)
			require.NoError(t, err)
			reply := adapter.Execute(t.Context(), req)
			if mode == "oversized" || mode == "decoder-oversized" {
				require.Equal(t, operator.Unknown, reply.Transport)
				require.Equal(t, "truncated-output", reply.UncertaintyRule)
				return
			}
			require.Equal(t, operator.Succeeded, reply.Transport)
			var batch operator.HostReadBatchOutput
			require.NoError(t, observe.Decode(reply.Payload, &batch, observe.Contract{"replies"}))
			require.Len(t, batch.Replies, 5)
			for _, child := range batch.Replies {
				require.Equal(t, operator.Succeeded, child.Transport)
				var host operator.HostOutput
				require.NoError(t, json.Unmarshal(child.Payload, &host))
				want := output
				if mode == "exact-limit" && child.ID == "relay" {
					want = strings.Repeat("A", 1002) + output[1002:]
				}
				require.Equal(t, want, host.Stdout)
			}
			if mode == "twice-packed" {
				require.GreaterOrEqual(t, len(reply.Payload), 2*len(packed))
			}
			if mode == "exact-limit" {
				require.Len(t, reply.Payload, 256000)
			}
			// Prove the enlarged reply also fits the unchanged public paid-path
			// codec, including frame metadata and its newline.
			var wire bytes.Buffer
			frame := operator.Frame{Type: "result", Session: strings.Repeat("s", 128), Result: &reply}
			require.NoError(t, operatortransport.NewCodec(nil, &wire).Write(frame))
			require.LessOrEqual(t, wire.Len(), operator.MaxFrameBytes)
			decoded, err := operatortransport.NewCodec(&wire, nil).Read()
			require.NoError(t, err)
			require.JSONEq(t, string(reply.Payload), string(decoded.Result.Payload))
			t.Logf("five-host packed need=%d, accepted payload=%d, frame limit=%d", len(packed), len(reply.Payload), operator.MaxFrameBytes)
		})
	}
}
