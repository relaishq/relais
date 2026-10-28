package observe

import "embed"

// DecoderSources is the same standard-library-only decoder used by pinned
// judges built against release snapshots predating the operator packages.
//
//go:embed strict.go canonical.go presence.go numbers.go required.go evidence_optional.go
var DecoderSources embed.FS
