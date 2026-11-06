package clockprobe

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecordLineRoundTripAndFixedLayout(t *testing.T) {
	for _, target := range []Target{Control, WorkerOne, WorkerTwo} {
		for _, result := range []Result{Success, RequestFailed, InvalidSample} {
			r := Record{target, 1793930000000000000, 1793930000000100000, 1793930000000101000, 1793930000000200000, result}
			line, err := r.MarshalLine()
			require.NoError(t, err)
			require.Len(t, line, LineBytes)
			require.Equal(t, byte('0'+target), line[0])
			require.Equal(t, "1793930000000000000", string(line[2:21]))
			require.Equal(t, byte('0'+result), line[82])
			parsed, err := ParseLine(line)
			require.NoError(t, err)
			require.Equal(t, r, parsed)
		}
	}
	marker := Record{Target: capacityTarget, Result: CapacityReached}
	line, err := marker.MarshalLine()
	require.NoError(t, err)
	require.Equal(t, "3 "+strings.Repeat("0", 19)+" "+strings.Repeat("0", 19)+" "+strings.Repeat("0", 19)+" "+strings.Repeat("0", 19)+" 3\n", string(line))
	parsed, err := ParseLine(line)
	require.NoError(t, err)
	require.Equal(t, marker, parsed)
}

func TestRecordParserWaitsForPartialLastLine(t *testing.T) {
	line, err := (Record{Target: WorkerOne, Result: RequestFailed}).MarshalLine()
	require.NoError(t, err)
	for n := 0; n < len(line); n++ {
		_, err := ParseLine(line[:n])
		require.ErrorIs(t, err, io.ErrUnexpectedEOF, "prefix length %d", n)
	}
	_, err = ParseLine(line)
	require.NoError(t, err)
}

func TestRecordParserRejectsMalformedCompleteLines(t *testing.T) {
	line, err := (Record{Target: Control, Result: Success}).MarshalLine()
	require.NoError(t, err)
	for _, i := range []int{0, 1, 2, 21, 41, 61, 81, 82, 83} {
		bad := bytes.Clone(line)
		bad[i] = 'x'
		_, err := ParseLine(bad)
		require.Error(t, err, "byte %d", i)
	}
	for _, bad := range [][]byte{[]byte("short\n"), append(bytes.Clone(line), 'x'), bytes.Replace(line, []byte(strings.Repeat("0", 19)), []byte("9999999999999999999"), 1)} {
		_, err := ParseLine(bad)
		require.Error(t, err)
	}
	for _, r := range []Record{{Target: 4}, {Result: 4}, {FrontendSend: -1}, {Target: capacityTarget}, {Result: CapacityReached}, {Target: capacityTarget, Result: CapacityReached, HostSend: 1}} {
		_, err := r.MarshalLine()
		require.Error(t, err)
	}
}
