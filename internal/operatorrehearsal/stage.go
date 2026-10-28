package operatorrehearsal

import (
	"context"
	"errors"
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
			// Docker archive-copy and the running systemd host can see different
			// /tmp mounts. Keep the input beside the template's persistent paths.
			target := "/opt/relais/relais-stage-" + pin.Version + ".tar.gz"
			destination := "/opt/relais/preview/" + pin.Version
			paths := fmt.Sprintf("host=%s host_source=%q copy_destination=%s:%s download_source=file://%s stage_destination=%s expected_sha256=%s", slot, archive, id, target, target, destination, pin.ArchiveSHA256)
			_, _ = fmt.Fprintln(r.diagnostics, r.topology.Redact("release-stage "+paths))
			if _, e := r.command(ctx, "stage-copy", "docker", "cp", archive, id+":"+target); e != nil {
				_, _ = fmt.Fprintln(r.diagnostics, r.topology.Redact("release-stage-copy-failed "+paths))
				return errors.New("release-stage-copy-failed")
			}
			script := fmt.Sprintf("set -eu\nRELAIS_TEST_ARCHIVE_URL=%s /opt/relais/boot/download.sh %s x86_64 %s %s 'worker relay control signal'\nprintf '%%s' %s > %s\nrm -- %s\n", quote("file://"+target), quote(pin.Version), quote(pin.ArchiveSHA256), quote(destination), quote(pin.ArchiveSHA256), quote(destination+"/archive.sha256"), quote(target))
			if e := r.deployment.shell(ctx, slot, "stage-release", script); e != nil {
				_, _ = fmt.Fprintln(r.diagnostics, r.topology.Redact("release-stage-failed "+paths))
				return errors.New("release-stage-failed")
			}
		}
	}
	return nil
}
