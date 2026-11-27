package artifacts

import (
	"context"
	"reflect"

	"github.com/relaishq/relais/internal/releasecompat"
)

// PlanSingle verifies one published release with the same archive, policy and
// stable-metadata checks as the pair planner. It makes no upgrade/rollback plan.
func PlanSingle(ctx context.Context, reader Reader, request PinRequest, arch string) (Pin, error) {
	if _, err := releasecompat.Compare(request.Version, request.Version); err != nil {
		return Pin{}, refuse("version-invalid", "expected a release version without v prefix")
	}
	if !stableVersion.MatchString(request.Version) {
		return Pin{}, refuse("version-unstable", "expected a stable release")
	}
	if c, _ := releasecompat.Compare(request.Version, "0.2.0"); c <= 0 {
		return Pin{}, refuse("version-unsupported", "release must be newer than 0.2.0")
	}
	if arch != "x86_64" && arch != "arm64" {
		return Pin{}, refuse("architecture-invalid", "architecture must be arm64 or x86_64")
	}
	if !shaPattern.MatchString(request.ArchiveSHA256) {
		return Pin{}, refuse("planned-digest-invalid", "planned archive SHA-256 must be lowercase hex")
	}
	pin, _, snap, err := resolve(ctx, reader, request, arch)
	if err != nil {
		return Pin{}, err
	}
	release, err := releaseView(ctx, reader, snap.tag)
	if err != nil {
		return Pin{}, err
	}
	assets, err := assetList(ctx, reader, snap.tag)
	if err != nil {
		return Pin{}, err
	}
	if release != snap.release || !reflect.DeepEqual(assets, snap.assets) {
		return Pin{}, refuse("release-changed", "release metadata or assets changed while planning")
	}
	return pin, nil
}
