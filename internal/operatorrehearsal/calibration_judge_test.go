package operatorrehearsal

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
)

var calibrationJudgeFixture struct {
	once  sync.Once
	root  string
	judge operatorevidence.PinnedJudge
	err   error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if calibrationJudgeFixture.root != "" {
		_ = os.RemoveAll(calibrationJudgeFixture.root)
	}
	os.Exit(code)
}

func testCalibrate(t *testing.T, raw string) (Calibration, error) {
	t.Helper()
	f := &calibrationJudgeFixture
	f.once.Do(func() {
		f.root, f.err = os.MkdirTemp("", "relais-calibration-test-")
		if f.err != nil {
			return
		}
		var compiler, checkout string
		compiler, f.err = exec.LookPath("go")
		if f.err != nil {
			return
		}
		checkout, f.err = filepath.Abs("../..")
		if f.err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		f.judge, f.err = operatorevidence.BuildPinned(ctx, checkout, ReleaseACommit, compiler, filepath.Join(f.root, "judge"))
	})
	if f.err != nil {
		t.Fatal(f.err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return Calibrate(ctx, f.judge, raw)
}

func TestCalibrationUsesFirstAndFinalSampleRules(t *testing.T) {
	// A first interval over 1.025s remains flagged, but most of it precedes
	// first video. The zero-packet final sample is valid and outside media.
	c, err := testCalibrate(t, calibrationShape(60, 1202877166, time.Second, false))
	if err != nil || !c.Passed || c.Samples != 61 || c.FlaggedPercent >= 1 {
		t.Fatal(c, err)
	}
	// The judge has no 30-sample, 30-second or two-GOMAXPROCS sample rule.
	c, err = testCalibrate(t, strings.ReplaceAll(calibrationShape(2, time.Second, 0, false), `"gomaxprocs":2`, `"gomaxprocs":1`))
	if err != nil || !c.Passed || c.Samples != 3 {
		t.Fatal("calibration added a local minimum", c, err)
	}
}
