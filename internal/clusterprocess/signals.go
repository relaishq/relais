package clusterprocess

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// Manager owns every service and the throwaway Redis across trials.
// Start and forced shutdown share a lock so a signal cannot miss a new child.
type Manager struct {
	mu       sync.Mutex
	children []*Child
	stopping bool
}

func (m *Manager) Start(dir, name string, env []string, args ...string) (*Child, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping {
		return nil, context.Canceled
	}
	c, err := Start(dir, name, env, args...)
	if err == nil {
		m.children = append(m.children, c)
	}
	return c, err
}

func (m *Manager) Stop(force bool) {
	m.mu.Lock()
	m.stopping = true
	children := append([]*Child(nil), m.children...)
	if force {
		for _, c := range children {
			_ = c.SignalGroup(syscall.SIGKILL)
		}
	}
	m.mu.Unlock()
	for i := len(children) - 1; i >= 0; i-- {
		c := children[i]
		if force {
			<-c.done
			_ = c.log.Close()
		} else {
			c.Stop()
		}
	}
}

// Context keeps notification active during cleanup. The first signal cancels
// work; a second kills and reaps every owned child before exiting. Workers use
// processrun.Context instead, retaining their immediate second-signal exit.
func (m *Manager) Context() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			m.Stop(false)
			signal.Stop(signals)
			close(done)
		})
	}
	go func() {
		select {
		case <-signals:
			cancel()
		case <-done:
			return
		}
		select {
		case sig := <-signals:
			m.Stop(true)
			os.Exit(128 + int(sig.(syscall.Signal)))
		case <-done:
		}
	}()
	return ctx, stop
}
