package inventory

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/operatorgates/oracletest"
	"github.com/relaishq/relais/internal/releasecompat"
)

func TestInventoryOracle(t *testing.T) {
	rows := oracletest.Cases(t, "inventory.parseInventory")
	for i, c := range rows {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			slot := oracletest.JSON[string](t, c.Request.Inputs[0])
			text := oracletest.JSON[string](t, c.Request.Inputs[1])
			var encoded struct {
				Encoding string          `json:"encoding"`
				Value    json.RawMessage `json:"value"`
			}
			if err := json.Unmarshal([]byte(text), &encoded); err != nil {
				t.Fatal(err)
			}
			pins := oracletest.JSON[[]Pin](t, c.Request.Inputs[2])
			got, err := Parse(slot, oracletest.ProductJSON(t, encoded.Value), pins)
			if (err == nil) != (c.Result.Completion == "returned") {
				t.Fatalf("oracle inventory verdict: %s: %v", c.Scenario, err)
			}
			if err == nil {
				want := oracletest.JSON[[]releasecompat.Participant](t, oracletest.ProductJSON(t, c.Result.Value))
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("oracle inventory value differs: %s", c.Scenario)
				}
			}
		})
	}
	t.Logf("replayed %d inventory invocations", len(rows))
}

func TestInventoryFailureCode(t *testing.T) {
	if got := Unavailable("w2", "INVENTORY_UNKNOWN:not-running\n").Error(); got != "inventory-unavailable:w2-not-running" {
		t.Fatalf("inventory-category-not-forwarded: %s", got)
	}
	if strings.Contains(Unavailable("w2", "secret arbitrary output").Error(), "secret") {
		t.Fatal("inventory free text escaped")
	}
}

func TestBootHashMatchesPublicTemplate(t *testing.T) {
	template, err := os.ReadFile("../../../deploy/aws/preview.yaml")
	if err != nil {
		t.Fatal(err)
	}
	boot, err := os.ReadFile("../../../deploy/aws/boot/run.sh")
	if err != nil {
		t.Fatal(err)
	}
	got, err := BootHash(string(template))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x", sha256.Sum256(boot))
	if got != want {
		t.Fatalf("template boot identity differs: %s != %s", got, want)
	}
	if _, err := BootHash("missing block"); err == nil {
		t.Fatal("missing boot block accepted")
	}
}

func TestInventoryUsesNativeManifestVersionGuard(t *testing.T) {
	for _, c := range oracletest.Cases(t, "inventory.parseInventory") {
		if c.Result.Completion != "returned" {
			continue
		}
		var encoded struct {
			Value struct {
				ObservedAt   int64                        `json:"observedAt"`
				Participants []map[string]json.RawMessage `json:"participants"`
			} `json:"value"`
		}
		_ = json.Unmarshal([]byte(oracletest.JSON[string](t, c.Request.Inputs[1])), &encoded)
		for _, p := range encoded.Value.Participants {
			var manifest map[string]json.RawMessage
			_ = json.Unmarshal(p["manifest"], &manifest)
			manifest["manifest_version"] = json.RawMessage(`3`)
			p["manifest"] = oracletest.Marshal(t, manifest)
		}
		_, err := Parse(oracletest.JSON[string](t, c.Request.Inputs[0]), oracletest.ProductJSON(t, oracletest.Marshal(t, encoded.Value)), oracletest.JSON[[]Pin](t, c.Request.Inputs[2]))
		var refusal *releasecompat.Refusal
		if !errors.As(err, &refusal) || refusal.Code != "unsupported_manifest" {
			t.Fatalf("inventory bypassed native manifest version guard: %v", err)
		}
		return
	}
	t.Fatal("missing valid inventory fixture")
}
