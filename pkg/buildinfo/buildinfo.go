package buildinfo

// These variables can be set via -ldflags at build time, e.g.:
// -ldflags "-X github.com/relais/pkg/buildinfo.Version=v0.1.0 -X github.com/relais/pkg/buildinfo.Commit=$(GIT_COMMIT) -X github.com/relais/pkg/buildinfo.Date=$(DATE)"
var (
	Version = ""
	Commit  = ""
	Date    = ""
)

// Fields returns non-empty build info fields for structured logging.
func Fields() map[string]interface{} {
	m := map[string]interface{}{}
	if Version != "" {
		m["build.version"] = Version
	}
	if Commit != "" {
		m["build.commit"] = Commit
	}
	if Date != "" {
		m["build.date"] = Date
	}
	return m
}
