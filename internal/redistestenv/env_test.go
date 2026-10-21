package redistestenv

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Child failures exercise testing.TB.Fatal without making network connections.
func TestEndpointPolicy(t *testing.T) {
	if os.Getenv("RELAIS_ENDPOINT_CHILD") == "1" {
		Address(t)
		ClusterAddresses(t)
		return
	}
	for _, tc := range []struct {
		name, addr, seeds, optIn string
		wantOK                   bool
	}{
		{name: "unset", wantOK: true},
		{name: "dedicated", addr: "127.0.0.1:16379", wantOK: true},
		{name: "reserved", addr: "127.0.0.1:6379"},
		{name: "padded-port", addr: "localhost:06379"},
		{name: "explicit-opt-in", addr: "127.0.0.1:6379", optIn: "1"},
		{name: "remote", addr: "cache.example.invalid:6379"},
		{name: "address-list", addr: "127.0.0.1:16379,127.0.0.1:16380"},
		{name: "address-whitespace", addr: " 127.0.0.1:16379 "},
		{name: "malformed", addr: "not-an-address"},
		{name: "seeds", seeds: "127.0.0.1:16379, [::1]:16380", wantOK: true},
		{name: "remote-seed", seeds: "127.0.0.1:16379,cache.example.invalid:6379"},
		{name: "opt-in-seed", seeds: "127.0.0.1:6379", optIn: "1"},
		{name: "reserved-seed", seeds: "cache.example.invalid:16379,[::1]:6379"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestEndpointPolicy$")
			for _, value := range os.Environ() {
				if !strings.HasPrefix(value, "RELAIS_TEST_REDIS_ADDR=") &&
					!strings.HasPrefix(value, "RELAIS_TEST_REDIS_CLUSTER_ADDRS=") &&
					!strings.HasPrefix(value, "RELAIS_REDIS_ALLOW_LOCAL_6379=") {
					cmd.Env = append(cmd.Env, value)
				}
			}
			cmd.Env = append(cmd.Env, "RELAIS_ENDPOINT_CHILD=1", "RELAIS_TEST_REDIS_ADDR="+tc.addr,
				"RELAIS_TEST_REDIS_CLUSTER_ADDRS="+tc.seeds, "RELAIS_REDIS_ALLOW_LOCAL_6379="+tc.optIn)
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.wantOK {
				t.Fatalf("success=%v, want %v: %s", err == nil, tc.wantOK, out)
			}
			if err != nil && !strings.Contains(string(out), "dedicated test Redis") {
				t.Fatalf("missing repair guidance: %s", out)
			}
		})
	}
}
