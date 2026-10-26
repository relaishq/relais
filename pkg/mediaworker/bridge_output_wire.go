package mediaworker

import (
	"encoding/base64"
	"io"
	"net/http"
)

// The exact image already owns its encoded bytes. Stream the private reply's
// base64 wrapper through the encoder's fixed 1 KiB buffer instead of allocating
// another whole-image JSON buffer. ExportReply's wire bytes remain identical.
// The fixed move-cost margin includes this buffer and HTTP framing.
func writeBridgeExportReply(w http.ResponseWriter, state []byte) error {
	w.Header().Set("Content-Type", "application/json")
	if _, err := io.WriteString(w, `{"state":"`); err != nil {
		return err
	}
	encoder := base64.NewEncoder(base64.StdEncoding, w)
	if _, err := encoder.Write(state); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\"}\n")
	return err
}
