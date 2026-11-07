package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/clockprobe"
	"github.com/stretchr/testify/require"
)

func TestPythonProbeParserMatchesGo(t *testing.T) {
	var lines []string
	for target := clockprobe.Control; target <= clockprobe.WorkerTwo; target++ {
		for result := clockprobe.Success; result <= clockprobe.InvalidSample; result++ {
			line, err := (clockprobe.Record{Target: target, FrontendSend: 1793930000000000000, Result: result}).MarshalLine()
			require.NoError(t, err)
			lines = append(lines, string(line))
		}
	}
	lines = append(lines, "3 0000000000000000000 0000000000000000000 0000000000000000000 0000000000000000000 3\n")
	good := lines[0]
	for i := 0; i < len(good); i++ {
		lines = append(lines, good[:i])
	}
	for _, at := range []int{0, 1, 2, 21, 41, 61, 81, 82, 83} {
		bad := []byte(good)
		bad[at] = 'x'
		lines = append(lines, string(bad))
	}
	lines = append(lines, "bad\n", "0 9999999999999999999 0000000000000000000 0000000000000000000 0000000000000000000 0\n")
	input, err := json.Marshal(lines)
	require.NoError(t, err)
	cmd := exec.Command("python3", "-B", "-c", `import clock,json,sys
out=[]
for line in json.load(sys.stdin):
 try:
  value=clock.probe_line(line.encode())
  out.append('partial' if value is None else 'valid')
 except ValueError:
  out.append('invalid')
print(json.dumps(out))`)
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	var results []string
	require.NoError(t, json.Unmarshal(output, &results))
	require.Len(t, results, len(lines))
	for i, line := range lines {
		_, err := clockprobe.ParseLine([]byte(line))
		want := "valid"
		if errors.Is(err, io.ErrUnexpectedEOF) {
			want = "partial"
		} else if err != nil {
			want = "invalid"
		}
		require.Equal(t, want, results[i], "line %d", i)
	}
}

// Execute the generated Python verbatim. Only the host commands and wall clock
// are replaced, so this test does not depend on a local chrony installation.
func TestGeneratedClockWithProbesPreservesBaseline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probes")
	line, err := (clockprobe.Record{Target: clockprobe.WorkerOne, FrontendSend: 1793930000000000000, FrontendReceive: 1793930000000001000, Result: clockprobe.RequestFailed}).MarshalLine()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, line, 0600))
	run := func(script string) map[string]json.RawMessage {
		body, ok := strings.CutPrefix(script, "python3 - <<'PY'\n")
		require.True(t, ok)
		body, ok = strings.CutSuffix(body, "PY\n")
		require.True(t, ok)
		cmd := exec.Command("python3", "-B", "-c", `import sys,subprocess
from unittest.mock import patch
sample='System time : 0.000001 seconds\nRoot dispersion : 0.000002 seconds\nRoot delay : 0.000004 seconds\nResidual freq : 0.1 ppm\nSkew : 0.2 ppm\nLeap status : Normal\n'
def command(args, **kwargs):
 if args == ['/usr/bin/chronyc', 'tracking']:
  return subprocess.CompletedProcess(args, 0, sample, '')
 raise OSError('no local chrony daemon')
with patch('subprocess.run', side_effect=command), patch('time.time_ns', return_value=1793930000000000000):
 exec(compile(sys.stdin.read(), '<generated-clock>', 'exec'))`)
		cmd.Stdin = strings.NewReader(body)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		raw, err := Unpack(string(out), 256000)
		require.NoError(t, err, string(out))
		var result map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &result))
		return result
	}
	baseline := run(Clock(nil, 0))
	withProbes := run(ClockWithProbes(nil, 0, path, nil))
	var batch struct {
		Lines string `json:"lines"`
	}
	require.NoError(t, json.Unmarshal(withProbes["probeLog"], &batch))
	require.Equal(t, string(line), batch.Lines)
	delete(withProbes, "probeLog")
	require.Equal(t, baseline, withProbes)
	withLease := run(ClockWithLease(nil, 0, path, nil))
	delete(withLease, "lease")
	delete(withLease, "probeLog")
	require.Equal(t, baseline, withLease, "unavailable optional lease changed baseline")
}
