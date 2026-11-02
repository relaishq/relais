// Package relaytrace provides an opt-in, in-process relay timeline probe.
// It retains no payload, credentials or keys; observers must return promptly.
package relaytrace

import (
	"sync"
	"sync/atomic"
	"time"
)

type Observer func(time.Time, string, string, []byte)

var observer atomic.Pointer[Observer]
var scope sync.Mutex

// Observe serializes measurement scopes. Production never enables a scope.
func Observe(fn Observer) func() {
	scope.Lock()
	observer.Store(&fn)
	var once sync.Once
	return func() { once.Do(func() { observer.Store(nil); scope.Unlock() }) }
}

// Enabled lets packet paths avoid constructing metadata outside a probe.
func Enabled() bool { return observer.Load() != nil }

// Emit exposes borrowed bytes only for the duration of the observer call.
func Emit(name, path string, packet []byte) {
	if fn := observer.Load(); fn != nil {
		(*fn)(time.Now(), name, path, packet)
	}
}
