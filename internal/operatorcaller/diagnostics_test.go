package operatorcaller

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
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
