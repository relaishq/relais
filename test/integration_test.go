package test

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

// TestStoredFrameFlow preserves frame persistence and metadata coverage.
func TestStoredFrameFlow(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStorage()
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	want := []storage.Frame{
		{SessionID: "test_camera", Index: 0, Data: []byte("first frame"), Timestamp: time.Unix(1, 0), MediaType: "video", Codec: "jpeg", KeyFrame: true},
		{SessionID: "test_camera", Index: 1, Data: []byte("second frame"), Timestamp: time.Unix(2, 0), MediaType: "video", Codec: "jpeg", KeyFrame: true},
	}
	for _, frame := range want {
		require.NoError(t, store.PutFrame(ctx, frame))
	}

	got, err := store.ListFrames(ctx, "test_camera")
	require.NoError(t, err)
	require.Equal(t, want, got)
}
