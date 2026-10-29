package workerlink_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"testing"

	"github.com/relaishq/relais/internal/workerlink"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
)

func TestLinkContractGolden(t *testing.T) {
	entries := map[string]string{}
	for _, m := range []workerlink.Message{
		{Kind: "request", ID: 7, Timeout: 123456789, Method: "POST", Path: "/sessions/call/export", Epoch: 9},
		{Kind: "reply", ID: 7, Status: 200, Body: []byte(`{"state":"AQID"}`)},
		{Kind: "request", ID: 8, Timeout: 123456789, Method: "POST", Path: "/resume", Body: []byte("chunk"), More: true},
		{Kind: "ping"},
		{Kind: "reply", ID: 9, Error: "not_applied"},
		{Kind: "reply", ID: 10, Error: "uncertain"},
	} {
		var b bytes.Buffer
		if err := workerlink.Write(&b, m); err != nil {
			t.Fatal(err)
		}
		entries[fmt.Sprintf("%s/%d", m.Kind, m.ID)] = hex.EncodeToString(b.Bytes())
		got, err := workerlink.Read(&b)
		if err != nil || got.Kind != m.Kind || got.ID != m.ID || !bytes.Equal(got.Body, m.Body) {
			t.Fatal("link reader/writer disagreement")
		}
	}

	for kind, value := range map[string]any{
		"hello":   controlplane.LinkHello{Instance: "instance", Sequence: 2, Release: "0.0.0-test", Contract: workerlink.ContractVersion, DeathCycle: 3, Status: mediaworker.WorkerStatus{InventoryKnown: true, Inventory: []mediaworker.InstalledSession{{ID: "call", Epoch: 9, Owner: netip.MustParseAddrPort("192.0.2.1:17001")}}}},
		"welcome": controlplane.LinkWelcome{Token: "00112233445566778899aabbccddeeff", DeathCycle: 3},
		"address": controlplane.LinkAddress{Address: netip.MustParseAddrPort("192.0.2.1:17001")},
		"ready":   struct{}{},
	} {
		var b bytes.Buffer
		if err := workerlink.WriteValue(&b, kind, value); err != nil {
			t.Fatal(err)
		}
		entries[kind] = hex.EncodeToString(b.Bytes())
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	path := fmt.Sprintf("testdata/contract-v%d.json", workerlink.ContractVersion)
	if os.Getenv("RELAIS_UPDATE_LINK_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	old, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(old, data) {
		t.Fatal("link contract drift: bump ContractVersion and generate a new golden")
	}
}
