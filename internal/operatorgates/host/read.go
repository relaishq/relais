package host

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

func Unpack(stdout string, limit int64) ([]byte, error) {
	compressed, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(stdout))
	if err != nil {
		return nil, err
	}
	z, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	defer z.Close()
	data, err := io.ReadAll(io.LimitReader(z, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("host evidence exceeds limit")
	}
	return data, nil
}
