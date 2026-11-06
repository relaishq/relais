package clockprobe

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

const MaxFileBytes = 4 << 20

// ErrFull means the capacity marker is present and no more probes can be saved.
var ErrFull = errors.New("clock probe log full")

// Log serializes appends from the three targets and reserves one capacity line.
type Log struct {
	mu   sync.Mutex
	file *os.File
	size int64
	full bool
	err  error
}

func OpenLog(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	l := &Log{file: f}
	if err := l.resume(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return l, nil
}

func (l *Log) resume() error {
	info, err := l.file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxFileBytes {
		return fmt.Errorf("clock probe file must be a regular file of at most 4 MiB")
	}
	l.size = info.Size()
	tail := make([]byte, min(l.size, LineBytes))
	if _, err := l.file.ReadAt(tail, l.size-int64(len(tail))); err != nil && err != io.EOF {
		return err
	}
	marker, _ := (Record{Target: capacityTarget, Result: CapacityReached}).MarshalLine()
	l.full = bytes.Equal(tail, marker)
	fragment := tail[bytes.LastIndexByte(tail, '\n')+1:]
	if len(fragment) > 0 && len(fragment) < LineBytes && bytes.HasPrefix(marker, fragment) && l.size+int64(LineBytes-len(fragment)) <= MaxFileBytes {
		l.full = true
		return l.write(marker[len(fragment):])
	}
	// A crash can leave a partial append. Preserve it as one malformed line so
	// later complete records remain readable; the reader records the loss.
	if !l.full && len(tail) > 0 && tail[len(tail)-1] != '\n' && l.size < MaxFileBytes-LineBytes {
		return l.write([]byte{'\n'})
	}
	return nil
}

func (l *Log) write(line []byte) error {
	n, err := l.file.Write(line)
	l.size += int64(n)
	if err == nil && n != len(line) {
		err = io.ErrShortWrite
	}
	l.err = err
	return err
}

func (l *Log) Append(r Record) error {
	line, err := r.MarshalLine()
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	if l.full {
		return ErrFull
	}
	if l.size+2*LineBytes > MaxFileBytes {
		l.full = true
		if l.size+LineBytes > MaxFileBytes {
			return fmt.Errorf("clock probe file has no room for capacity marker")
		}
		line, _ = (Record{Target: capacityTarget, Result: CapacityReached}).MarshalLine()
		if err := l.write(line); err != nil {
			return err
		}
		return ErrFull
	}
	return l.write(line)
}

func (l *Log) Close() error { return l.file.Close() }
