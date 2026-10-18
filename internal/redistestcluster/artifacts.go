package redistestcluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Directory keeps failure/timeout evidence in a known directory. Successful
// tests remove their own directory. It deliberately does not use t.TempDir:
// timeout exit and panic evidence must survive the test binary.
func Directory(t *testing.T, kind string) string {
	t.Helper()
	root := artifactRoot()
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	name := strings.NewReplacer("/", "-", "\\", "-").Replace(t.Name())
	dir, err := os.MkdirTemp(root, kind+"-"+name+"-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("failure artifacts retained at %s", dir)
			return
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove owned test directory: %v", err)
		}
	})
	return dir
}
func artifactRoot() string {
	if root := os.Getenv("RELAIS_TEST_ARTIFACT_DIR"); root != "" {
		return root
	}
	return filepath.Join(os.TempDir(), "relais-cluster-artifacts")
}
