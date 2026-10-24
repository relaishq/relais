package relayleg_test

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/relaishq/relais/internal/relayleg"
	"github.com/relaishq/relais/pkg/relay"
)

func TestUDPContractGolden(t *testing.T) {
	entries := map[string]string{}
	for _, ack := range []bool{false, true} {
		packet := relayleg.Barrier(0x0102030405060708, ack)
		id, got, ok := relayleg.ParseBarrier(packet)
		if !ok || got != ack || id != 0x0102030405060708 {
			t.Fatal("barrier reader/writer disagreement")
		}
		entries[fmt.Sprintf("drain-ack=%t", ack)] = hex.EncodeToString(packet)
	}
	for _, address := range []string{"192.0.2.1:1234", "[2001:db8::1]:5678", "[::ffff:192.0.2.1]:1234"} {
		caller := netip.MustParseAddrPort(address)
		packet := append(relay.AppendHeader(nil, caller), 0x80, 0x6f, 0x12, 0x34)
		got, payload, err := relay.ParseHeader(packet)
		if err != nil || got != netip.AddrPortFrom(caller.Addr().Unmap(), caller.Port()) || hex.EncodeToString(payload) != "806f1234" {
			t.Fatal("caller header reader/writer disagreement")
		}
		entries[address] = hex.EncodeToString(packet)
	}

	for name, raw := range map[string]string{
		"unknown-caller-version": "020404d2c0000201806f1234",
		"unknown-caller-family":  "010504d2c0000201806f1234",
		"truncated-caller":       "010404",
	} {
		packet, err := hex.DecodeString(raw)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = relay.ParseHeader(packet)
		entries["parse/"+name] = raw + fmt.Sprintf(" accepted=%t", err == nil)
	}
	for name, raw := range map[string]string{
		"unknown-magic": "524c5302000102030405060708",
		"unknown-ack":   "524c5301020102030405060708",
		"truncated":     "524c53010001020304050607",
	} {
		packet, err := hex.DecodeString(raw)
		if err != nil {
			t.Fatal(err)
		}
		_, _, ok := relayleg.ParseBarrier(packet)
		entries["parse/"+name] = raw + fmt.Sprintf(" accepted=%t", ok)
	}
	checkContractGolden(t, fmt.Sprintf("testdata/contract-v%d.json", relayleg.ContractVersion), entries, "RELAIS_UPDATE_UDP_GOLDEN")
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
