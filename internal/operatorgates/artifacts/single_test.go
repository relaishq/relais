package artifacts

import (
	"context"
	"testing"
)

func TestSingleReleasePlan(t *testing.T) {
	r, q := setup(t)
	pin, err := PlanSingle(context.Background(), r, q.From, q.Arch)
	if err != nil || pin.Version != q.From.Version || pin.ArchiveSHA256 != q.From.ArchiveSHA256 || r.reads["v0.3.0"] != 2 || r.reads["v0.3.1"] != 0 {
		t.Fatal(pin, err, r.reads)
	}
	// The existing pair planner must still reject a single release.
	q.To = q.From
	if _, err = PlanArtifacts(context.Background(), r, q); code(err) != "version-same" {
		t.Fatal(err)
	}
	for _, mode := range []string{"digest", "manifest", "changed", "platform"} {
		t.Run(mode, func(t *testing.T) {
			r, q := setup(t)
			arch := q.Arch
			switch mode {
			case "digest":
				r.data["v0.3.0/relais_0.3.0_linux_arm64.tar.gz"][0] ^= 1
			case "manifest":
				delete(r.files, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/scripts/release/compatibility.json")
			case "changed":
				r.hook = func(tag string) {
					if r.reads[tag] == 2 {
						reply := r.releases[tag]
						reply.Release.TagCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
						r.releases[tag] = reply
					}
				}
			case "platform":
				arch = "bad"
			}
			if _, err := PlanSingle(context.Background(), r, q.From, arch); err == nil {
				t.Fatal("single release accepted invalid evidence")
			}
		})
	}
}
