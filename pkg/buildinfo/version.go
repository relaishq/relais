package buildinfo

import (
	"flag"
	"fmt"
	"io"
)

// VersionFlag registers the common version flag on an existing parser.
func VersionFlag(flags *flag.FlagSet) *bool {
	return flags.Bool("version", false, "print version, commit and build date, then exit")
}

// PrintVersion prints the linked build identity without opening any services.
func PrintVersion(w io.Writer, name string) error {
	value := func(s string) string {
		if s == "" {
			return "unknown"
		}
		return s
	}
	_, err := fmt.Fprintf(w, "%s version=%s commit=%s date=%s\n", name, value(Version), value(Commit), value(Date))
	return err
}
