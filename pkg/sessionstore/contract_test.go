package sessionstore_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/relaishq/relais/pkg/sessionstore"

	"github.com/relaishq/relais/internal/releasecompat"
)

func TestStoreScriptContractGolden(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("RELAIS_UPDATE_STORE_GOLDEN") == "1" {
		path := filepath.Join(root, "pkg/sessionstore/testdata/script-contract.json")
		data, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		var golden releasecompat.ScriptGolden
		if e = json.Unmarshal(data, &golden); e != nil {
			t.Fatal(e)
		}
		actual, e := releasecompat.LuaScripts(root)
		if e != nil {
			t.Fatal(e)
		}
		next := map[string]releasecompat.Script{}
		for name, sha := range actual {
			s := golden.Scripts[name]
			s.SHA = sha
			next[name] = s
		}
		golden.Contract = sessionstore.ScriptContractVersion
		golden.Scripts = next
		data, e = releasecompat.JSON(golden)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(path, data, 0644); e != nil {
			t.Fatal(e)
		}
	}
	if _, err = releasecompat.CheckStoreGolden(root); err != nil {
		t.Fatal(err)
	}
}
