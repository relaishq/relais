package clockprobe

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLogCapWritesOneMarkerAndSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probes")
	log, err := OpenLog(path)
	require.NoError(t, err)
	r := Record{Target: WorkerTwo, FrontendSend: 1, FrontendReceive: 2, Result: RequestFailed}
	for range MaxFileBytes/LineBytes + 10 {
		require.NoError(t, log.Append(r))
	}
	require.NoError(t, log.Close())
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.LessOrEqual(t, len(before), MaxFileBytes)
	require.Equal(t, MaxFileBytes/LineBytes*LineBytes, len(before))
	scanner := bufio.NewScanner(bytes.NewReader(before))
	markers := 0
	for scanner.Scan() {
		record, err := ParseLine(append(bytes.Clone(scanner.Bytes()), '\n'))
		require.NoError(t, err)
		if record.Result == CapacityReached {
			markers++
		} else {
			require.Equal(t, r, record)
		}
	}
	require.NoError(t, scanner.Err())
	require.Equal(t, 1, markers)
	log, err = OpenLog(path)
	require.NoError(t, err)
	require.NoError(t, log.Append(r))
	require.NoError(t, log.Close())
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestLogRestartAppendsAndSeparatesPartialTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probes")
	r := Record{Target: Control, FrontendSend: 1, FrontendReceive: 2, Result: RequestFailed}
	line, err := r.MarshalLine()
	require.NoError(t, err)
	log, err := OpenLog(path)
	require.NoError(t, err)
	require.NoError(t, log.Append(r))
	require.NoError(t, log.Close())
	log, err = OpenLog(path)
	require.NoError(t, err)
	require.NoError(t, log.Append(r))
	require.NoError(t, log.Close())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, bytes.Repeat(line, 2), data)
	// Preserve incomplete bytes as one malformed line, then resume whole lines.
	require.NoError(t, os.WriteFile(path, append(data, line[:20]...), 0600))
	log, err = OpenLog(path)
	require.NoError(t, err)
	require.NoError(t, log.Append(r))
	require.NoError(t, log.Close())
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	lines := bytes.SplitAfter(data, []byte{'\n'})
	require.Len(t, lines, 5)
	_, err = ParseLine(lines[2])
	require.Error(t, err)
	parsed, err := ParseLine(lines[3])
	require.NoError(t, err)
	require.Equal(t, r, parsed)
}

func TestLogSerializesConcurrentTargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probes")
	log, err := OpenLog(path)
	require.NoError(t, err)
	var writers sync.WaitGroup
	for _, target := range []Target{Control, WorkerOne, WorkerTwo} {
		writers.Go(func() {
			for i := range 100 {
				if err := log.Append(Record{Target: target, FrontendSend: int64(i), Result: RequestFailed}); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	writers.Wait()
	require.NoError(t, log.Close())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Len(t, data, 300*LineBytes)
	for start := 0; start < len(data); start += LineBytes {
		_, err := ParseLine(data[start : start+LineBytes])
		require.NoError(t, err)
	}
}

func TestLogRestartCompletesPartialCapacityMarkerOnce(t *testing.T) {
	marker, err := (Record{Target: capacityTarget, Result: CapacityReached}).MarshalLine()
	require.NoError(t, err)
	for _, n := range []int{1, 20, 82, 83} {
		path := filepath.Join(t.TempDir(), "probes")
		require.NoError(t, os.WriteFile(path, marker[:n], 0600))
		log, err := OpenLog(path)
		require.NoError(t, err)
		require.NoError(t, log.Append(Record{}))
		require.NoError(t, log.Close())
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, marker, data)
	}
}

func TestLogRejectsInvalidFilesAndRetainsWriteFailure(t *testing.T) {
	_, err := OpenLog(t.TempDir())
	require.Error(t, err)
	path := filepath.Join(t.TempDir(), "oversized")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(MaxFileBytes+1))
	require.NoError(t, f.Close())
	_, err = OpenLog(path)
	require.Error(t, err)
	log, err := OpenLog(filepath.Join(t.TempDir(), "closed"))
	require.NoError(t, err)
	require.NoError(t, log.Close())
	first := log.Append(Record{})
	require.Error(t, first)
	require.Equal(t, first, log.Append(Record{}))
}
