package logging

import (
	"os"

	"github.com/sirupsen/logrus"
)

// Logger wraps logrus.Logger to provide application-specific logging
type Logger struct {
	*logrus.Logger
}

// NewLogger creates a new configured logger
func NewLogger(level string) *Logger {
	logger := logrus.New()
	logger.SetOutput(os.Stdout)

	// Parse log level
	logLevel, err := logrus.ParseLevel(level)
	if err != nil {
		logLevel = logrus.InfoLevel
	}
	logger.SetLevel(logLevel)

	// Set formatter
	logger.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: "2006-01-02T15:04:05.000Z07:00",
	})

	return &Logger{
		Logger: logger,
	}
}
