package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestRedisKeyContractGolden(t *testing.T) {
	r := &Redis{prefix: "fixture:"}
	entries := map[string]any{}
	for _, id := range []string{"call", "call{tag}", "~call", "", "雪"} {
		entries["session/"+id] = r.keys(id)
		entries["components/"+id] = r.componentKeys(id)
		entries["lifecycle/"+id] = r.lifecycleKeys(id)
		entries["intent/"+id] = r.intentKey(id)
		entries["relay/"+id] = r.relayKey(id)
		for _, op := range []string{"clock", "sequence", "put", "inventory", "get"} {
			entries[op+"/"+id] = r.sessionKeys(id, op)
		}
	}
	entries["control"] = r.controlKey()
	capture := &epochKeyCapture{}
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer func() { _ = client.Close() }()
	client.AddHook(capture)
	r.client = client
	if _, err := r.allocate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if capture.key == "" {
		t.Fatal("epoch key was not captured")
	}
	entries["epoch_allocator"] = capture.key
	for _, worker := range []string{"192.0.2.1:1234", "[2001:db8::1]:1234"} {
		entries["worker/"+worker] = r.indexKey(netip.MustParseAddrPort(worker))
	}
	checkContractGolden(t, fmt.Sprintf("testdata/key-contract-v%d.json", KeyContractVersion), entries, "RELAIS_UPDATE_KEY_GOLDEN")
}

func checkContractGolden(t *testing.T, path string, value any, update string) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if os.Getenv(update) == "1" {
		if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	old, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(old) != string(data) {
		t.Fatalf("contract fixture drift: bump the contract version and generate the versioned golden with %s=1", update)
	}
}

// Capture the actual INCR argument without dialing a Redis endpoint.
type epochKeyCapture struct{ key string }

func (*epochKeyCapture) DialHook(redis.DialHook) redis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("key fixture must never dial")
	}
}
func (h *epochKeyCapture) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		if cmd.Name() != "incr" || len(cmd.Args()) != 2 {
			return errors.New("unexpected fixture command")
		}
		h.key = fmt.Sprint(cmd.Args()[1])
		cmd.(*redis.IntCmd).SetVal(1)
		return nil
	}
}
func (*epochKeyCapture) ProcessPipelineHook(redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(context.Context, []redis.Cmder) error { return errors.New("unexpected fixture pipeline") }
}
