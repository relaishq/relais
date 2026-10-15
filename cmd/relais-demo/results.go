package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/relaishq/relais/internal/privateapi"
)

const maxResultsBody = 8 << 20

func (d *demo) prepareResults(dir string) error {
	if dir == "" {
		return nil
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("-results-dir must be a directory")
	}
	d.resultsDir = absolute
	return nil
}

func (d *demo) uploadResults(w http.ResponseWriter, r *http.Request) {
	if d.resultsDir == "" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "results must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxResultsBody))
	if err != nil {
		var size *http.MaxBytesError
		if errors.As(err, &size) {
			http.Error(w, "results body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "cannot read results", http.StatusBadRequest)
		}
		return
	}
	var record map[string]json.RawMessage
	if json.Unmarshal(data, &record) != nil || record == nil || len(bytes.TrimSpace(data)) == 0 {
		http.Error(w, "expected one JSON results object", http.StatusBadRequest)
		return
	}
	name, err := d.writeResults(data)
	if err != nil {
		http.Error(w, "cannot persist results", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	privateapi.Write(w, map[string]string{"file": name})
}

func (d *demo) writeResults(data []byte) (string, error) {
	d.resultsMu.Lock()
	defer d.resultsMu.Unlock()
	for {
		d.resultsNext++
		name := fmt.Sprintf("results-%s-%d.json", d.launchID, d.resultsNext)
		path := filepath.Join(d.resultsDir, name)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, writeErr := file.Write(data)
		if writeErr == nil {
			writeErr = file.Sync()
		}
		err = errors.Join(writeErr, file.Close())
		if err != nil {
			_ = os.Remove(path)
			return "", err
		}
		return name, nil
	}
}
