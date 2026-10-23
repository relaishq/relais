// Package rediswiretest reads RESP2 messages for socket-level fault tests.
package rediswiretest

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Read returns one complete response or command, including nested arrays.
func Read(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 3 {
		return nil, fmt.Errorf("invalid RESP line")
	}
	switch line[0] {
	case '+', '-', ':':
		return line, nil
	case '$':
		n, err := strconv.Atoi(strings.TrimSpace(string(line[1:])))
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return line, nil
		}
		data := make([]byte, n+2)
		_, err = io.ReadFull(r, data)
		return append(line, data...), err
	case '*':
		n, err := strconv.Atoi(strings.TrimSpace(string(line[1:])))
		if err != nil {
			return nil, err
		}
		for i := 0; i < n; i++ {
			part, err := Read(r)
			if err != nil {
				return nil, err
			}
			line = append(line, part...)
		}
		return line, nil
	default:
		return nil, fmt.Errorf("unsupported RESP2 type %q", line[0])
	}
}

// Command decodes the bulk-string arguments of one command.
func Command(raw []byte) ([]string, error) {
	r := bufio.NewReader(bytes.NewReader(raw))
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
	if err != nil {
		return nil, err
	}
	args := make([]string, n)
	for i := range args {
		line, err = r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "$")))
		if err != nil || size < 0 {
			return nil, fmt.Errorf("invalid command length")
		}
		data := make([]byte, size+2)
		if _, err = io.ReadFull(r, data); err != nil {
			return nil, err
		}
		args[i] = string(data[:size])
	}
	return args, nil
}
