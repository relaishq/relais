package sessionstore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	codec "github.com/relaishq/relais/internal/componentcodec"
	"github.com/relaishq/relais/internal/rediswiretest"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

type storeWireCommand struct {
	request, reply []byte
	args           []string
}
type storeWireRecorder struct {
	mu       sync.Mutex
	commands []storeWireCommand
}

func (w *storeWireRecorder) take() []storeWireCommand {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.commands
	w.commands = nil
	return out
}

// Put a transparent RESP proxy in front of the disposable endpoint. NewRedis
// builds the real production client, including its normal connection options.
func recordedRedis(t *testing.T) (*Redis, *storeWireRecorder) {
	t.Helper()
	base := testRedis(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	wire := &storeWireRecorder{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	conns := map[net.Conn]bool{}
	closed := false
	t.Cleanup(func() {
		require.NoError(t, listener.Close())
		mu.Lock()
		closed = true
		for c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			front, e := listener.Accept()
			if e != nil {
				return
			}
			mu.Lock()
			if closed {
				_ = front.Close()
				mu.Unlock()
				return
			}
			conns[front] = true
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { _ = front.Close(); mu.Lock(); delete(conns, front); mu.Unlock() }()
				back, e := net.DialTimeout("tcp", base.client.(*redis.Client).Options().Addr, time.Second)
				if e != nil {
					return
				}
				defer func() { _ = back.Close(); mu.Lock(); delete(conns, back); mu.Unlock() }()
				mu.Lock()
				if closed {
					mu.Unlock()
					return
				}
				conns[back] = true
				mu.Unlock()
				in, out := bufio.NewReader(front), bufio.NewReader(back)
				for {
					raw, e := rediswiretest.Read(in)
					if e != nil {
						return
					}
					args, e := rediswiretest.Command(raw)
					if e != nil {
						return
					}
					if _, e = io.Copy(back, bytes.NewReader(raw)); e != nil {
						return
					}
					reply, e := rediswiretest.Read(out)
					if e != nil {
						return
					}
					wire.mu.Lock()
					wire.commands = append(wire.commands, storeWireCommand{raw, reply, args})
					wire.mu.Unlock()
					if _, e = io.Copy(front, bytes.NewReader(reply)); e != nil {
						return
					}
				}
			}()
		}
	}()
	r, err := NewRedis(context.Background(), storage.RedisConfig{Addr: listener.Addr().String(), Prefix: base.prefix}, make([]byte, 32))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	return r, wire
}

func respCommand(args []string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, s := range args {
		fmt.Fprintf(&b, "$%d\r\n", len(s))
		b.WriteString(s)
		b.WriteString("\r\n")
	}
	return b.Bytes()
}
func scriptCommands(calls []storeWireCommand, script *redis.Script, body string) []storeWireCommand {
	var out []storeWireCommand
	for _, c := range calls {
		if len(c.args) > 2 && (c.args[0] == "evalsha" && c.args[1] == script.Hash() || c.args[0] == "eval" && c.args[1] == body) {
			out = append(out, c)
		}
	}
	return out
}
func TestRedisRouteAndIndexScriptCache(t *testing.T) {
	r, wire := recordedRedis(t)
	ctx := context.Background()
	lease, err := r.Claim(ctx, "script-cache", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
	require.NoError(t, err)
	r.maintenance.Wait()
	route := routeFor(lease, netip.MustParseAddrPort("127.0.0.1:34567"), 30*time.Second)
	for _, tc := range []struct {
		name, body string
		script     *redis.Script
		call       func() error
		saving     int
	}{
		{"route", putRouteLua, putRouteScript, func() error { return r.PutRoute(ctx, route, false) }, 2401},
		{"index", indexLua, indexScript, func() error { return r.indexContext(ctx, lease, time.Minute) }, 210},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, r.client.ScriptFlush(ctx).Err())
			wire.take()
			require.NoError(t, tc.call())
			cold := scriptCommands(wire.take(), tc.script, tc.body)
			require.Len(t, cold, 2)
			require.Equal(t, "evalsha", cold[0].args[0])
			require.Contains(t, string(cold[0].reply), "NOSCRIPT")
			require.Equal(t, "eval", cold[1].args[0])
			require.Equal(t, tc.body, cold[1].args[1])
			require.NoError(t, tc.call())
			warm := scriptCommands(wire.take(), tc.script, tc.body)
			require.Len(t, warm, 1)
			require.Equal(t, "evalsha", warm[0].args[0])
			old := append([]string(nil), warm[0].args...)
			old[0], old[1] = "eval", tc.body
			require.Equal(t, tc.saving, len(respCommand(old))-len(warm[0].request))
			require.Equal(t, warm[0].reply, cold[1].reply)
			t.Logf("warm request: old=%d new=%d saved=%d; cold requests=%d", len(respCommand(old)), len(warm[0].request), tc.saving, len(cold[0].request)+len(cold[1].request))
		})
	}
}

func TestRedisComponentCommitWireBytes(t *testing.T) {
	r, wire := recordedRedis(t)
	ctx := context.Background()
	const id = "wire-session-001"
	lease, err := r.Claim(ctx, id, netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
	require.NoError(t, err)
	r.maintenance.Wait()
	require.NoError(t, r.client.HSet(ctx, r.keys(id)[0], "next_seq", "99").Err())
	require.NoError(t, r.client.ScriptLoad(ctx, componentCommitLua).Err())
	c := componentFixture(id)
	c.Work, err = codec.Marshal(codec.Work, make([]byte, 3072))
	require.NoError(t, err)
	c.History, err = codec.Marshal(codec.History, make([]byte, 8192))
	require.NoError(t, err)
	var previous *ComponentMetadata
	for _, tc := range []struct {
		name          string
		kind          byte
		saving, delta int
	}{
		{"initialization", 0, 46, 246}, {"host-only", codec.Transport, 251, 42},
		{"work-only", codec.Work, 165, 127}, {"history-only", codec.History, 165, 127},
		{"both-app", codec.Root, 124, 168}, {"rotation", 255, 83, 209},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.kind == codec.Work || tc.kind == codec.Root {
				c.Work = changeComponentByte(t, c.Work, codec.Work)
			}
			if tc.kind == codec.History || tc.kind == codec.Root {
				c.History = changeComponentByte(t, c.History, codec.History)
			}
			if tc.kind == 255 {
				r.keysByID[1] = bytes.Repeat([]byte{1}, 32)
				r.activeKey = 1
			}
			c.Transport = advanceComponentTransport(t, c.Transport)
			wire.take()
			tuple, e := r.CommitComponents(ctx, lease, ComponentCommit{Components: c, StateVersion: 1, Expected: previous})
			require.NoError(t, e)
			calls := scriptCommands(wire.take(), componentCommitScript, componentCommitLua)
			require.Len(t, calls, 1)
			call := calls[0]
			require.Equal(t, "evalsha", call.args[0])
			numKeys, e := strconv.Atoi(call.args[2])
			require.NoError(t, e)
			require.Equal(t, 11, numKeys)
			args := call.args[3+numKeys:]
			require.Len(t, args, 6)
			packed, e := tuple.Metadata.Marshal()
			require.NoError(t, e)
			require.Len(t, args[1], 20+tc.delta)
			epoch, seq := tuple.Metadata.OwnerEpoch, tuple.Metadata.References[4].Revision
			old := append([]string(nil), call.args[:3+numKeys]...)
			// SHA length is fixed at 40; its content changes no wire-byte count.
			old = append(old, tuple.Metadata.Owner, strconv.FormatUint(epoch, 10), args[0], string(packed), args[2], strconv.FormatUint(seq, 10), string(binary.BigEndian.AppendUint64(nil, epoch)), args[3], args[4], args[5], string(binary.BigEndian.AppendUint64(nil, seq)))
			oldBytes := respCommand(old)
			require.Equal(t, call.request, respCommand(call.args), "recorded production bytes include all RESP framing")
			saved := len(oldBytes) - len(call.request)
			require.Equal(t, tc.saving, saved)
			// Independent length model includes five removed arguments and the binding.
			bulk := func(n int) int { return n + len(strconv.Itoa(n)) + 5 }
			model := bulk(len(tuple.Metadata.Owner)) + bulk(len(strconv.FormatUint(epoch, 10))) + bulk(len(strconv.FormatUint(seq, 10))) + 2*bulk(8) + bulk(len(packed)) - bulk(20+tc.delta)
			require.Equal(t, model, saved)
			restored, e := r.ReadComponents(ctx, id)
			require.NoError(t, e)
			require.Equal(t, packed, mustPackMetadata(t, restored.Metadata), "Lua publishes exactly what the client authenticated")
			require.Equal(t, c, restored.Components)
			t.Logf("old request=%d new request=%d reply=%d saved=%d model=%d", len(oldBytes), len(call.request), len(call.reply), saved, model)
			previous = &tuple.Metadata
		})
	}
}
func mustPackMetadata(t *testing.T, m ComponentMetadata) []byte {
	t.Helper()
	b, err := m.Marshal()
	require.NoError(t, err)
	return b
}

func changeComponentByte(t *testing.T, data []byte, kind byte) []byte {
	t.Helper()
	var plain []byte
	require.NoError(t, codec.Unmarshal(data, kind, &plain))
	require.NotEmpty(t, plain)
	plain[0] ^= 1
	encoded, err := codec.Marshal(kind, plain)
	require.NoError(t, err)
	return encoded
}
