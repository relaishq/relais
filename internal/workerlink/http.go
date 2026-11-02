package workerlink

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/relaishq/relais/internal/privateapi"
)

func fmtEpoch(epoch uint64) string   { return strconv.FormatUint(epoch, 10) }
func parseEpoch(value string) uint64 { epoch, _ := strconv.ParseUint(value, 10, 64); return epoch }

// Transport uses standard-library prior-knowledge cleartext HTTP/2. Heartbeats
// use a different ordinary HTTP client and therefore a separate connection.
func Transport() *http.Transport {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Transport{Protocols: protocols, HTTP2: &http.HTTP2Config{SendPingTimeout: PingInterval, PingTimeout: LostAfter - PingInterval}}
}

// WriteValue and ReadValue bound registration messages by the same wire cap.
func WriteValue(w io.Writer, kind string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > privateapi.MaxBody {
		return errors.New("workerlink: hello too large")
	}
	for {
		n := min(len(data), ChunkSize)
		if err = Write(w, Message{Kind: kind, Body: data[:n], More: len(data) > n}); err != nil {
			return err
		}
		if len(data) <= n {
			return nil
		}
		data = data[n:]
	}
}
func ReadValue(r io.Reader, kind string, value any) error {
	var data []byte
	for {
		m, err := Read(r)
		if err != nil {
			return err
		}
		if m.Kind != kind {
			return errors.New("workerlink: unexpected handshake")
		}
		data = append(data, m.Body...)
		if len(data) > privateapi.MaxBody {
			return errors.New("workerlink: hello too large")
		}
		if !m.More {
			return json.Unmarshal(data, value)
		}
	}
}
