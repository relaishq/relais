package storage

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestWithRetry_SucceedsAfterTransientErrors(t *testing.T) {
	s := &RedisStorage{}
	ctx := context.Background()
	op := "pipeline_exec"

	attempts := 0
	err := s.withRetry(ctx, op, func() error {
		attempts++
		if attempts < 3 {
			return timeoutErr{}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success, got err=%v", err)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
}

func TestWithRetry_NonRetryableError(t *testing.T) {
	s := &RedisStorage{}
	ctx := context.Background()
	op := "pipeline_exec"

	attempts := 0
	err := s.withRetry(ctx, op, func() error {
		attempts++
		return errors.New("WRONGTYPE Operation against a key holding the wrong kind of value")
	})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if attempts != 1 {
		t.Fatalf("expected 1 attempt for non-retryable error, got %d", attempts)
	}
}

// ensure context cancel stops retries early and doesn't bump counters further
func TestWithRetry_ContextCanceled(t *testing.T) {
	s := &RedisStorage{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	op := "pipeline_exec"

	attempts := 0
	// Err value doesn't matter; wrapper should return immediately after first err due to context
	err := s.withRetry(ctx, op, func() error { attempts++; return timeoutErr{} })
	if err == nil {
		t.Fatalf("expected error due to canceled context")
	}
	// allow any timers to tick (no effect on attempts)
	time.Sleep(5 * time.Millisecond)
	if attempts != 1 {
		t.Fatalf("expected 1 attempt due to canceled context, got %d", attempts)
	}
}

func TestClassifyRedisErr(t *testing.T) {
	if got := classifyRedisErr(nil); got != "" {
		t.Fatalf("nil -> %q", got)
	}
	if got := classifyRedisErr(context.Canceled); got != "canceled" {
		t.Fatalf("canceled -> %q", got)
	}
	var ne net.Error = timeoutErr{}
	if got := classifyRedisErr(ne); got != "timeout" {
		t.Fatalf("timeout -> %q", got)
	}
	if got := classifyRedisErr(errors.New("MOVED 3999 127.0.0.1:7002")); got != "moved" {
		t.Fatalf("moved -> %q", got)
	}
	if got := classifyRedisErr(errors.New("ASK 3999 127.0.0.1:7002")); got != "ask" {
		t.Fatalf("ask -> %q", got)
	}
	if got := classifyRedisErr(errors.New("TRYAGAIN")); got != "tryagain" {
		t.Fatalf("tryagain -> %q", got)
	}
	if got := classifyRedisErr(errors.New("BUSY")); got != "busy" {
		t.Fatalf("busy -> %q", got)
	}
	if got := classifyRedisErr(errors.New("CLUSTERDOWN Hash slot not served")); got != "clusterdown" {
		t.Fatalf("clusterdown -> %q", got)
	}
	if got := classifyRedisErr(errors.New("unexpected EOF")); got != "eof" {
		t.Fatalf("eof -> %q", got)
	}
}
