package operatorrehearsal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/relaishq/relais/internal/operatorgates/artifacts"
)

const (
	ReleaseRVersion = "0.4.0"
	ReleaseRCommit  = "ebb0f7f19595cf62840e6a923c74b6314b6df1c7"
	ReleaseRSHA256  = "0b8e61ef0228018d9e2f7a82bdae1f3763fa1992456aa2207ece82a07b8dfe0e"
)

// PrepareRelayLossRelease is the token-consuming R stage. Its own directory
// and single pin leave the A/B/M pair preparation and plan bytes unchanged.
func PrepareRelayLossRelease(ctx context.Context, directory, token string) error {
	r := &archiveReader{Reader: artifacts.NewPublicReader(nil, token), archives: map[string][]byte{}}
	return prepareRelayLossRelease(ctx, directory, r)
}
func prepareRelayLossRelease(ctx context.Context, directory string, r *archiveReader) error {
	pin, err := artifacts.PlanSingle(ctx, r, artifacts.PinRequest{Version: ReleaseRVersion, ArchiveSHA256: ReleaseRSHA256}, "x86_64")
	if err != nil {
		return err
	}
	if err = validateRelayLossPin(pin); err != nil {
		return err
	}
	if err = os.Mkdir(directory, 0700); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(directory, pin.Archive), r.archives[pin.Archive], 0600); err != nil {
		return err
	}
	raw, err := json.Marshal(pin)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, "release-r.json"), raw, 0600)
}
func validateRelayLossPin(p artifacts.Pin) error {
	if p.Version != ReleaseRVersion || p.Tag != "v"+ReleaseRVersion || p.Arch != "x86_64" || p.Commit != ReleaseRCommit || p.Archive != "relais_0.4.0_linux_amd64.tar.gz" || p.ArchiveSHA256 != ReleaseRSHA256 || !fullDigest.MatchString(p.ManifestSHA256) {
		return &artifacts.Refusal{Code: "release-profile-differs", Detail: "release differs from the pinned part R release"}
	}
	return nil
}
func readRelayLossRelease(directory string) (artifacts.Pin, error) {
	var pin artifacts.Pin
	raw, err := os.ReadFile(filepath.Join(directory, "release-r.json"))
	if err != nil {
		return pin, err
	}
	if err = json.Unmarshal(raw, &pin); err != nil {
		return pin, err
	}
	if err = validateRelayLossPin(pin); err != nil {
		return pin, err
	}
	raw, err = os.ReadFile(filepath.Join(directory, pin.Archive))
	if err != nil {
		return pin, err
	}
	if hash(raw) != pin.ArchiveSHA256 {
		return pin, errors.New("release-digest-differs")
	}
	manifest, err := artifacts.ArchiveManifest(raw, "relais_"+pin.Version+"_linux_amd64")
	if err != nil || hash(manifest) != pin.ManifestSHA256 {
		return pin, errors.New("release-manifest-differs")
	}
	return pin, nil
}
