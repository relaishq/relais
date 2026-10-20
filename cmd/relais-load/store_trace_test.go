package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func wireCommand(args ...string) wireValue {
	var b bytes.Buffer
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, arg := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(arg), arg)
	}
	v, err := readWire(bufio.NewReader(&b), 0)
	if err != nil {
		panic(err)
	}
	return v
}
func TestStoreTraceCountsAllAttemptedBytes(t *testing.T) {
	keys := []string{"p:lease:{sess:x}", "p:record:{sess:x}", "p:epoch:{sess:x}", "p:routes:{sess:x}", "p:ended:{sess:x}", "p:setup:{sess:x}", "p:checkpoint:{sess:x}", "p:work:{sess:x}:0", "p:work:{sess:x}:1", "p:history:{sess:x}:0", "p:history:{sess:x}:1"}
	args := append([]string{"evalsha", "sha", "11"}, keys...)
	args = append(args, "owner", "1", "expected", "metadata", "encrypted-checkpoint", "1", "epoch", "encrypted-setup", "encrypted-work", "encrypted-history", "seqbytes")
	req := wireCommand(args...)
	reply, err := readWire(bufio.NewReader(strings.NewReader("*1\r\n:1\r\n")), 0)
	require.NoError(t, err)
	w := newWireWindow()
	w.observe(req, reply, nil, time.Millisecond)
	require.Len(t, w.KeyArguments, 11)
	require.EqualValues(t, 1, w.Components["history"].Writes)
	require.EqualValues(t, len(req.raw), w.RequestBytes)
	require.EqualValues(t, len(reply.raw), w.ReplyBytes)
	require.EqualValues(t, 8, w.CheckpointHSETFields["t"].Bytes)
	require.Equal(t, len("encrypted-checkpoint")+len("metadata")+8+3+len("encrypted-work"), w.Transactions["published_hot"].Max)
	failure, _ := readWire(bufio.NewReader(strings.NewReader("-NOSCRIPT missing\r\n")), 0)
	w.observe(req, failure, nil, 2*time.Millisecond)
	w.observe(req, wireValue{}, errors.New("lost reply"), time.Second)
	require.EqualValues(t, 3, w.WriteAttempts)
	require.EqualValues(t, 2, w.WriteFailures)
	require.Equal(t, 1, w.Components["work"].Writes)
	require.EqualValues(t, 3*len(req.raw), w.RequestBytes)
	args[0] = "eval"
	w.observe(wireCommand(args...), reply, nil, time.Millisecond)
	require.EqualValues(t, 1, w.ScriptBodies)
}
func TestRESPAndMonitorBinaryFrames(t *testing.T) {
	binary := string([]byte{0, '\r', '\n', 255, '"', '\\'})
	v := wireCommand("set", "x", binary)
	require.Equal(t, binary, string(v.array[2].text))
	args, err := monitorArguments(`+123.000 [0 lua] "HSET" "p:checkpoint:{sess:x}" "m" "\x00\r\n\xff\"\\"`)
	require.NoError(t, err)
	require.Equal(t, binary, args[3])
	for _, bad := range []string{"$4\r\nx\r\n", "*999999\r\n", "$-5\r\n", "_\r\n"} {
		_, err = readWire(bufio.NewReader(strings.NewReader(bad)), 0)
		require.Error(t, err)
	}
	_, err = monitorArguments(`+123 [0 lua] "HSET`)
	require.Error(t, err)
}
func TestObserverForwardsPipelineAndStops(t *testing.T) {
	backend, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer backend.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 2 {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					cmd, err := readWire(r, 0)
					if err != nil {
						return
					}
					if string(cmd.array[0].text) == "MONITOR" {
						_, _ = c.Write([]byte("+OK\r\n"))
						_, _ = r.ReadByte()
						return
					}
					_, _ = c.Write([]byte("+PONG\r\n"))
				}
			}()
		}
	}()
	o, err := startStoreObserver(backend.Addr().String())
	require.NoError(t, err)
	client, err := net.Dial("tcp4", o.listener.Addr().String())
	require.NoError(t, err)
	req := wireCommand("ping")
	_, err = client.Write(append(bytes.Clone(req.raw), req.raw...))
	require.NoError(t, err)
	r := bufio.NewReader(client)
	for range 2 {
		reply, err := readWire(r, 0)
		require.NoError(t, err)
		require.Equal(t, "PONG", string(reply.text))
	}
	require.NoError(t, client.Close())
	trace := o.close()
	<-done
	require.EqualValues(t, 2, trace.Phases["setup"].Commands)
	require.Positive(t, trace.PhaseSeconds["setup"])
	require.Empty(t, trace.Errors)
}

func TestScriptProtocolCountersDistinguishBodySubmissions(t *testing.T) {
	w := newWireWindow()
	reply, _ := readWire(bufio.NewReader(strings.NewReader(":1\r\n")), 0)
	req := wireCommand("eval", "return 1", "0")
	w.observe(req, reply, nil, time.Millisecond)
	w.observe(req, reply, nil, time.Millisecond)
	require.EqualValues(t, 2, w.ScriptBodies)
	require.Zero(t, w.ScriptLoads)
	w.observe(wireCommand("script", "load", "return 1"), reply, nil, time.Millisecond)
	require.EqualValues(t, 3, w.ScriptBodies)
	require.EqualValues(t, 1, w.ScriptLoads)
	missing, _ := readWire(bufio.NewReader(strings.NewReader("-NOSCRIPT missing\r\n")), 0)
	w.observe(wireCommand("evalsha", "sha", "0"), missing, nil, time.Millisecond)
	require.EqualValues(t, 1, w.NoScriptReplies)
}
