package config

import (
	"crypto/subtle"
	"errors"
	"strings"
)

func IsValidBearerToken(authorization, expectedToken string) bool {
	if !strings.HasPrefix(authorization, "Bearer ") || expectedToken == "" {
		return false
	}
	supplied := authorization[len("Bearer "):]
	return IsValidSecret(supplied, expectedToken)
}

func IsValidSecret(supplied, expected string) bool {
	if len(supplied) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(supplied), []byte(expected)) == 1
}

type BitrixValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

type AppError struct {
	Message    string
	Code       string
	StatusCode int
	OAuthCode  string
	Validation []BitrixValidationError
	Cause      error
}

func (e *AppError) Error() string {
	return e.Message
}

func (e *AppError) Unwrap() error {
	return e.Cause
}

func NewAppError(message, code string, statusCode int) *AppError {
	if statusCode == 0 {
		statusCode = 400
	}
	return &AppError{Message: message, Code: code, StatusCode: statusCode}
}

func NewNotFoundError(message string) *AppError {
	return &AppError{Message: message, Code: "NOT_FOUND", StatusCode: 404}
}

func NewPolicyError(message string) *AppError {
	return &AppError{Message: message, Code: "POLICY_DENIED", StatusCode: 403}
}

func NewAuthenticationError(message string) *AppError {
	if message == "" {
		message = "Unauthorized"
	}
	return &AppError{Message: message, Code: "UNAUTHORIZED", StatusCode: 401}
}

func SafeErrorMessage(err error) string {
	var appErr *AppError
	if errors.As(err, &appErr) {
		return appErr.Message
	}
	return "An unexpected internal error occurred"
}
