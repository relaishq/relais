package buildinfo

import (
	"bytes"
	"flag"
	"testing"
)

func TestVersionFlagAndOutput(t *testing.T) {
	oldVersion, oldCommit, oldDate := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = oldVersion, oldCommit, oldDate })
	for _, tc := range []struct{ version, commit, date, want string }{
		{"1.2.3", "abc123", "2026-10-15", "relais-worker version=1.2.3 commit=abc123 date=2026-10-15\n"},
		{"", "", "", "relais-worker version=unknown commit=unknown date=unknown\n"},
	} {
		Version, Commit, Date = tc.version, tc.commit, tc.date
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		version := VersionFlag(fs)
		if *version {
			t.Fatal("version must default to false")
		}
		if err := fs.Parse([]string{"-version"}); err != nil || !*version {
			t.Fatalf("version parse: %v", err)
		}
		var out bytes.Buffer
		if err := PrintVersion(&out, "relais-worker"); err != nil {
			t.Fatal(err)
		}
		if out.String() != tc.want {
			t.Fatalf("got %q want %q", out.String(), tc.want)
		}
	}
}
