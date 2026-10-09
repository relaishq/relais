package callharness

import (
	"fmt"

	"github.com/pion/logging"
)

// srtpScope is the logger scope of Pion's SRTP and SRTCP sessions. Pion
// reports a packet it cannot decrypt only by logging it at Info level on this
// scope (srtp session read loop), so the caller counts those messages.
const srtpScope = "srtp"

// callerLoggerFactory is the caller's Pion logger factory. It forwards to
// Pion's default loggers and reports SRTP decryption failures to the
// recorder.
type callerLoggerFactory struct {
	base logging.LoggerFactory
	rec  *recorder
}

func newCallerLoggerFactory(rec *recorder) *callerLoggerFactory {
	return &callerLoggerFactory{base: logging.NewDefaultLoggerFactory(), rec: rec}
}

func (f *callerLoggerFactory) NewLogger(scope string) logging.LeveledLogger {
	logger := f.base.NewLogger(scope)
	if scope != srtpScope {
		return logger
	}

	return &srtpLogger{LeveledLogger: logger, rec: f.rec}
}

type srtpLogger struct {
	logging.LeveledLogger
	rec *recorder
}

func (l *srtpLogger) Info(msg string) {
	l.rec.srtpError(msg)
	l.LeveledLogger.Info(msg)
}

func (l *srtpLogger) Infof(format string, args ...any) {
	l.rec.srtpError(fmt.Sprintf(format, args...))
	l.LeveledLogger.Infof(format, args...)
}
