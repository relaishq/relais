package operatorcaller

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestCallerFailureCodesOmitRawDetails(t *testing.T) {
	for _, row := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("caller-signaling-probe: %w", x509.UnknownAuthorityError{}), "caller-signaling-untrusted-certificate"},
		{fmt.Errorf("caller-signaling-probe: %w", x509.HostnameError{}), "caller-signaling-certificate-name"},
		{fmt.Errorf("caller-signaling-probe: %w", x509.CertificateInvalidError{}), "caller-signaling-certificate-invalid"},
		{errors.New("caller-signaling-http-401"), "caller-signaling-http-401"},
		{errors.New("caller-signaling-probe: private endpoint and token"), "caller-signaling-unavailable"},
		{errors.Join(errors.New("caller-stream-failed"), errors.New("caller-evidence-incomplete")), "caller-stream-failed"},
		{&os.PathError{Op: "open", Path: "/private-data", Err: os.ErrNotExist}, "caller-file-unavailable"},
		{context.DeadlineExceeded, "caller-deadline"},
		{errors.New("private endpoint and token"), "caller-operation-failed"},
		{errors.New("caller-signaling-http-401 secret"), "caller-operation-failed"},
	} {
		if got := FailureCode(row.err); got != row.want {
			t.Fatalf("caller diagnostic = %q; want %q", got, row.want)
		}
	}
}

func TestCallerFailureKeepsFileIdentityAndPrimaryExit(t *testing.T) {
	var out bytes.Buffer
	err := &os.PathError{Op: "open", Path: "/owned/caller.jsonl", Err: os.ErrNotExist}
	WriteFailure(&out, err)
	for _, want := range []string{"caller-file-unavailable", `operation="open"`, `path="/owned/caller.jsonl"`, "reason=not-found"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal("file failure lost its reason or target", out.String())
		}
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 7")
	exit := cmd.Run()
	if got := FailureCode(errors.Join(exit, err)); got != "caller-load-exit-7" {
		t.Fatal("missing report hid the primary child failure", got)
	}
}
