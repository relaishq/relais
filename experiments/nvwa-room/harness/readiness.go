package harness

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (t *Topology) startupFailure(name string, cause error) error {
	b, err := os.ReadFile(filepath.Join(t.Dir, name+".log"))
	tail := "(no child log available)"
	if err == nil {
		if len(b) > 8192 {
			b = b[len(b)-8192:]
		}
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) > 20 {
			lines = lines[len(lines)-20:]
		}
		tail = strings.Join(lines, "\n")
	}
	if t.redisPassword != "" {
		tail = strings.ReplaceAll(tail, t.redisPassword, "<REDACTED>")
	}
	return fmt.Errorf("startup %s: %w; last child log lines:\n%s", name, cause, tail)
}
func (t *Topology) waitReady(ctx context.Context, name, addr string, budget time.Duration) error {
	ready, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for {
		// Return the first exited child, even if a later service is the current wait.
		for _, c := range t.children {
			select {
			case <-c.done:
				return t.startupFailure(c.Name, fmt.Errorf("owned child exited: %v", c.waitErr))
			default:
			}
		}
		conn, err := (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(ready, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ready.Done():
			return t.startupFailure(name, fmt.Errorf("readiness %s: %w", addr, ready.Err()))
		case <-time.After(50 * time.Millisecond):
		}
	}
}
