package privateapi

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
)

func TestUncertainErrorCodeAlwaysWins(t *testing.T) {
	retryable := errors.New("link unavailable")
	codes := map[string]error{"worker_link_unavailable": retryable, "uncertain": ErrUncertain}
	for range 256 {
		reply := httptest.NewRecorder()
		Error(reply, Uncertain(retryable), codes)
		var f Failure
		if err := json.Unmarshal(reply.Body.Bytes(), &f); err != nil {
			t.Fatal(err)
		}
		if f.Code != "uncertain" {
			t.Fatalf("uncertainty hidden by %q", f.Code)
		}
	}
}
