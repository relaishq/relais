package util

import (
	"fmt"
)

// ErrorType represents different categories of errors
type ErrorType string

const (
	ErrorTypeValidation ErrorType = "validation"
	ErrorTypeStorage    ErrorType = "storage"
	ErrorTypePlugin     ErrorType = "plugin"
	ErrorTypeNetwork    ErrorType = "network"
)

// Error represents an application-specific error
type Error struct {
	Type    ErrorType
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Type, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Type, e.Message)
}

// NewError creates a new application error
func NewError(errType ErrorType, message string, cause error) error {
	return &Error{
		Type:    errType,
		Message: message,
		Cause:   cause,
	}
}

// IsErrorType checks if an error is of a specific type
func IsErrorType(err error, errType ErrorType) bool {
	if appErr, ok := err.(*Error); ok {
		return appErr.Type == errType
	}
	return false
}
