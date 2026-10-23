package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
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
	// Packed v10 metadata is 231 bytes plus owner length. Its contents are
	// opaque to the observer; only the raw/delta envelope affects its size.
	metadata := append([]byte{10}, bytes.Repeat([]byte{'m'}, 235)...)
	checksum := sha1.Sum(metadata)
	for _, mode := range []string{"raw", "delta"} {
		t.Run(mode, func(t *testing.T) {
			expected := ""
			encoded := append(bytes.Clone(checksum[:]), metadata...)
			if mode == "delta" {
				expected = string(metadata)
				// Root and checkpoint references change; the full m length stays fixed.
				encoded = append(bytes.Clone(checksum[:]), 0x98)
				encoded = append(encoded, metadata[124:206]...)
			}
			assertSplitCommitTrace(t, []string{expected, string(encoded), "encrypted-checkpoint", "encrypted-setup", "encrypted-work", "encrypted-history"}, len(metadata))
		})
	}
}

func TestStoreTraceCountsElevenArgumentCommits(t *testing.T) {
	metadata := string(append([]byte{10}, bytes.Repeat([]byte{'m'}, 235)...))
	assertSplitCommitTrace(t, []string{"owner", "1", "expected", metadata, "encrypted-checkpoint", "1", "epoch", "encrypted-setup", "encrypted-work", "encrypted-history", "seqbytes"}, len(metadata))
}

func assertSplitCommitTrace(t *testing.T, commitArgs []string, metadataBytes int) {
	t.Helper()
	keys := []string{"p:lease:{sess:x}", "p:record:{sess:x}", "p:epoch:{sess:x}", "p:routes:{sess:x}", "p:ended:{sess:x}", "p:setup:{sess:x}", "p:checkpoint:{sess:x}", "p:work:{sess:x}:0", "p:work:{sess:x}:1", "p:history:{sess:x}:0", "p:history:{sess:x}:1"}
	args := append([]string{"evalsha", "sha", "11"}, keys...)
	args = append(args, commitArgs...)
	req := wireCommand(args...)
	reply, err := readWire(bufio.NewReader(strings.NewReader("*1\r\n:1\r\n")), 0)
	require.NoError(t, err)
	w := newWireWindow()
	w.observe(req, reply, nil, time.Millisecond)
	require.EqualValues(t, 1, w.WriteAttempts)
	require.Zero(t, w.WriteFailures)
	require.EqualValues(t, len(req.raw), w.RequestBytes)
	require.EqualValues(t, len(reply.raw), w.ReplyBytes)
	require.Equal(t, keys, w.KeyExamples)
	require.Len(t, w.KeyArguments, 11)
	keyRESPBytes := len(req.array[2].raw)
	for i, key := range keys {
		require.Equal(t, &byteSizes{Writes: 1, Bytes: int64(len(key)), Min: len(key), Max: len(key)}, w.KeyArguments[fmt.Sprintf("%02d", i+1)])
		keyRESPBytes += len(req.array[3+i].raw)
	}
	require.EqualValues(t, keyRESPBytes, w.KeyRESPBytes)
	require.EqualValues(t, len(req.array[2].raw), w.NumkeysRESPBytes)
	require.Len(t, w.Components, 4)
	for _, name := range []string{"checkpoint", "setup", "work", "history"} {
		n := len("encrypted-" + name)
		require.Equal(t, &byteSizes{Writes: 1, Bytes: int64(n), Min: n, Max: n}, w.Components[name])
	}
	hot := len("encrypted-checkpoint") + metadataBytes + len("encrypted-work")
	total := hot + len("encrypted-setup") + len("encrypted-history")
	require.Equal(t, map[string]*byteSizes{
		"attempted_total": {Writes: 1, Bytes: int64(total), Min: total, Max: total},
		"published_hot":   {Writes: 1, Bytes: int64(hot), Min: hot, Max: hot},
		"published_total": {Writes: 1, Bytes: int64(total), Min: total, Max: total},
	}, w.Transactions)
	require.Equal(t, map[string]*byteSizes{
		"b":           {Writes: 1, Bytes: int64(len("encrypted-checkpoint")), Min: len("encrypted-checkpoint"), Max: len("encrypted-checkpoint")},
		"m":           {Writes: 1, Bytes: int64(metadataBytes), Min: metadataBytes, Max: metadataBytes},
		"t":           {Writes: 1, Bytes: 8, Min: 8, Max: 8},
		"field_names": {Writes: 3, Bytes: 3, Min: 1, Max: 1},
	}, w.CheckpointHSETFields)
	require.Equal(t, []float64{1}, w.CommitLatencyMS)
	failure, _ := readWire(bufio.NewReader(strings.NewReader("-NOSCRIPT missing\r\n")), 0)
	w.observe(req, failure, nil, 2*time.Millisecond)
	w.observe(req, wireValue{}, errors.New("lost reply"), time.Second)
	require.EqualValues(t, 3, w.WriteAttempts)
	require.EqualValues(t, 2, w.WriteFailures)
	require.Equal(t, 1, w.Components["work"].Writes)
	require.EqualValues(t, 3*len(req.raw), w.RequestBytes)
	require.Equal(t, &byteSizes{Writes: 3, Bytes: int64(3 * total), Min: total, Max: total}, w.Transactions["attempted_total"])
	require.Equal(t, 1, w.Transactions["published_total"].Writes)
	require.Equal(t, []float64{1}, w.CommitLatencyMS)
	args[0] = "eval"
	w.observe(wireCommand(args...), reply, nil, 3*time.Millisecond)
	require.EqualValues(t, 1, w.ScriptBodies)
	require.EqualValues(t, 4, w.WriteAttempts)
	require.Equal(t, []float64{1, 3}, w.CommitLatencyMS)
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

func TestStorePhaseIntervalsKeepSeparateSteadySegments(t *testing.T) {
	start := time.Now()
	o := &storeObserver{started: start, phaseAt: start, phase: "steady", trace: storeTrace{PhaseSeconds: map[string]float64{}, PhaseIntervals: map[string][]storePhaseInterval{}}}
	o.finishPhase(start.Add(6 * time.Second))
	o.phase = "move"
	o.finishPhase(start.Add(6500 * time.Millisecond))
	o.phase = "steady"
	o.finishPhase(start.Add(30 * time.Second))
	require.Equal(t, []storePhaseInterval{{Start: 0, End: 6}, {Start: 6.5, End: 30}}, o.trace.PhaseIntervals["steady"])
	require.Equal(t, 29.5, o.trace.PhaseSeconds["steady"])
	require.Equal(t, []storePhaseInterval{{Start: 6, End: 6.5}}, o.trace.PhaseIntervals["move"])
}
