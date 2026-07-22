package apperrors

import "errors"

var (
	ErrNotFound        = errors.New("resource not found")
	ErrVersionConflict = errors.New("version conflict")
	ErrStateConflict   = errors.New("state conflict")
)
