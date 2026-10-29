package workerapp

import (
	"context"
	"strings"
	"testing"
)

func TestWorkerConnectionModeDefaultsToInbound(t *testing.T) {
	flags, f := newWorkerFlags(Options{})
	if err := flags.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if *f.connect != "inbound" {
		t.Fatal("default connection mode changed")
	}
	if err := flags.Parse([]string{"-connect=dial-out"}); err != nil {
		t.Fatal(err)
	}
	if *f.connect != "dial-out" {
		t.Fatal("dial-out mode not selected")
	}
}
func TestWorkerConnectionModeRejectsInvalidConfiguration(t *testing.T) {

	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"-connect=poll"}, "-connect"},
		{[]string{"-connect=dial-out", "-name=a", "-control=http://127.0.0.1:1", "-relay-leg=127.0.0.1:2", "-relay-media=127.0.0.1:3", "-http=0.0.0.0:4"}, "loopback"},
	} {
		err := Run(context.Background(), test.args, Options{})
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("invalid configuration not rejected: %v", err)
		}
	}

}
