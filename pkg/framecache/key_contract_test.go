package framecache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/relaishq/relais/pkg/sessionstore"
)

func TestRedisKeyContractGolden(t *testing.T) {
	r := &Redis{prefix: "fixture:"}
	entries := map[string]string{}
	for _, id := range []string{"call", "call{tag}", "~call", "", "雪"} {
		entries[id] = r.key(id)
	}
	entries["track/audio"] = trackField(Track{Kind: "audio", SSRC: 42})
	entries["track/video"] = trackField(Track{Kind: "video", SSRC: 4294967295})
	checkContractGolden(t, fmt.Sprintf("testdata/key-contract-v%d.json", sessionstore.KeyContractVersion), entries, "RELAIS_UPDATE_KEY_GOLDEN")
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
