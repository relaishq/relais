package operatorrehearsal

import (
	"context"
	"fmt"
	"path/filepath"
)

// Stage both pinned releases with the real template downloader before the first
// media window. Runtime still checks each receipt/manifest and compatibility
// pool before effects. No network, extraction or build burst occurs in a gate.
func (r *rehearsal) stage(ctx context.Context) error {
	for _, slot := range Slots {
		id := r.deployment.ContainerIDs()[slot]
		for _, pin := range r.pins {
			archive := filepath.Join(r.c.Assets, "relais_"+pin.Version+"_linux_amd64.tar.gz")
			target := "/tmp/relais-stage-" + pin.Version + ".tar.gz"
			if _, e := r.command(ctx, "stage-copy", "docker", "cp", archive, id+":"+target); e != nil {
				return e
			}
			script := fmt.Sprintf("set -eu\nRELAIS_TEST_ARCHIVE_URL=file://%s /opt/relais/boot/download.sh %s x86_64 %s /opt/relais/preview/%s 'worker relay control signal'\nprintf '%%s' %s > /opt/relais/preview/%s/archive.sha256\nrm %s\n", target, pin.Version, pin.ArchiveSHA256, pin.Version, pin.ArchiveSHA256, pin.Version, target)
			if e := r.deployment.shell(ctx, slot, "stage-release", script); e != nil {
				return e
			}
		}
	}
	return nil
}
