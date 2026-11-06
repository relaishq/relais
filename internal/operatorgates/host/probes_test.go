package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
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
