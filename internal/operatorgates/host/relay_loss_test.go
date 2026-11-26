package host

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

// Captured from dd2eba5's renderer and assets before adding the R service steps.
func TestExistingHostScriptsStayByteIdentical(t *testing.T) {
	hashes := map[string]string{
		"status":          "165c943f5f935ed7bf8019cebf4a74d36e80d29c6b964cba28c9ca59f37628a2",
		"worker-down":     "cd00e94ff7ae998b7960c831ee6a91f4a60fda9dc1d6fc16a6b1e20788d0dccd",
		"worker-start":    "f072457968132e22f1f75568cda85c05acbbc59cbd5092fba33da9d869c80d50",
		"relay-stop":      "ce41a79e175184d60d74ac6f541f840713deaf46a5ddf51aa4f1839d68775602",
		"relay-replenish": "101c299e8dce302d9c7138be33f434a5da146d3367b99dbb94504a1552f51674",
		"relay-isolate":   "40efd805edaf16ea5ec0728a2df3433b5dc54a4d7914da0c145ce00e56a2f4e4",
		"relay-restore":   "70d71537b9a430444662c323cff4e563c1d6912d7224defcba7bd36940a6c524",
	}
	for op, want := range hashes {
		stage := &Stage{Version: "0.4.0", Arch: "arm64", ArchiveSHA256: strings.Repeat("a", 64), ManifestSHA256: strings.Repeat("b", 64), Pool: "e30=", Operation: "upgrade"}
		if IsRead(op) {
			stage = nil
		}
		script, err := Process(op, map[string]string{"worker": "w1"}, stage)
		if err != nil || fmt.Sprintf("%x", sha256.Sum256([]byte(script))) != want {
			t.Fatal(op, err, "existing host script bytes changed")
		}
	}
	stage := &Stage{Version: "0.4.0", Arch: "arm64", ArchiveSHA256: strings.Repeat("a", 64), ManifestSHA256: strings.Repeat("b", 64), Pool: "e30=", Operation: "upgrade"}
	for _, op := range []string{"relay-start", "relay-loss-stop"} {
		script, err := Process(op, map[string]string{"slot": "relay-two"}, stage)
		if err != nil || !strings.Contains(script, "data=relay_loss_execute(") {
			t.Fatal(op, err)
		}
	}
}
