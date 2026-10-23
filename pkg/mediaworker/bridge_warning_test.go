package mediaworker

import (
	"fmt"
	"sync"
	"testing"

	"github.com/pion/logging"
	"github.com/stretchr/testify/require"
)

type bridgeWarningLogger struct {
	logging.LeveledLogger
	mu       sync.Mutex
	warnings []string
}

func (l *bridgeWarningLogger) Warnf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warnings = append(l.warnings, fmt.Sprintf(format, args...))
}

type bridgeWarningFactory struct {
	logging.LoggerFactory
	logger *bridgeWarningLogger
}

func (f bridgeWarningFactory) NewLogger(scope string) logging.LeveledLogger {
	base := f.LoggerFactory.NewLogger(scope)
	if scope == "mediaworker" {
		f.logger.LeveledLogger = base
		return f.logger
	}
	return base
}
func TestBridgeStartupWarnsOnceForIncompatibleSplitBudgets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits SnapshotLimits
		flag   string
	}{
		{name: "defaults"},
		{name: "legacy-burst", limits: SnapshotLimits{BurstBytes: 28672}, flag: "-agent-snapshot-burst-bytes"},
		{name: "below-split-cadence", limits: SnapshotLimits{BytesPerSecond: 67583}, flag: "-agent-snapshot-bytes-per-second"},
		{name: "compatible-explicit-rate", limits: SnapshotLimits{BytesPerSecond: 78000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger := &bridgeWarningLogger{}
			w, err := New(Config{SnapshotLimits: tc.limits, LoggerFactory: bridgeWarningFactory{LoggerFactory: logging.NewDefaultLoggerFactory(), logger: logger}})
			require.NoError(t, err)
			require.NoError(t, w.Close())
			logger.mu.Lock()
			defer logger.mu.Unlock()
			if tc.flag == "" {
				require.Empty(t, logger.warnings)
			} else {
				require.Len(t, logger.warnings, 1)
				require.Contains(t, logger.warnings[0], "cannot adopt v10")
				require.Contains(t, logger.warnings[0], tc.flag)
			}
		})
	}
}
