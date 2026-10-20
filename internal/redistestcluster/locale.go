package redistestcluster

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
)

var utf8Locale struct {
	sync.Once
	name string
	err  error
}

// UTF8Environment deliberately excludes C.UTF-8: its byte collation hides
// locale-dependent Lua comparisons. An unavailable real UTF-8 locale is fatal.
func UTF8Environment(t *testing.T) []string {
	t.Helper()
	utf8Locale.Do(func() {
		out, err := exec.Command("locale", "-a").Output()
		if err != nil {
			utf8Locale.err = err
			return
		}
		for _, name := range strings.Fields(string(out)) {
			canonical := strings.ToLower(strings.ReplaceAll(name, "-", ""))
			if canonical == "en_us.utf8" {
				utf8Locale.name = name
				return
			}
		}
		utf8Locale.err = fmt.Errorf("en_US UTF-8 locale required; install/generate en_US.UTF-8 for Redis integration")
	})
	if utf8Locale.err != nil {
		t.Fatal(utf8Locale.err)
	}
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "LANG=") && !strings.HasPrefix(entry, "LC_ALL=") {
			env = append(env, entry)
		}
	}
	return append(env, "LANG="+utf8Locale.name, "LC_ALL="+utf8Locale.name)
}

// Standalone starts an owned, persistence-free UTF-8 Redis on a free port.
func Standalone(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("owned Redis process excluded from short mode")
	}
	dir := Directory(t, "standalone")
	addr, err := clusterprocess.FreeTCP()
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(addr)
	config := filepath.Join(dir, "redis.conf")
	if err = os.WriteFile(config, []byte("bind 127.0.0.1\nport "+port+"\nsave \"\"\nappendonly no\n"), 0600); err != nil {
		t.Fatal(err)
	}
	child, err := clusterprocess.StartSupervised(dir, "redis", UTF8Environment(t), "redis-server", config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(child.Stop)
	watchServer(t, child)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = child.WaitLog(ctx, "Ready to accept connections"); err != nil {
		t.Fatal(err)
	}
	t.Logf("owned Redis standalone port: %s", addr)
	return addr
}
