package sessionstore_test

import (
	"path/filepath"
	"testing"

	"github.com/relaishq/relais/internal/releasecompat"
)

func TestStoreScriptContractGolden(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = releasecompat.CheckStoreGolden(root); err != nil {
		t.Fatal(err)
	}
}
