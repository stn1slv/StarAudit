// Package history fetches the star history of a GitHub repository and
// detects star bursts in it.
package history

import "fmt"

// Error codes reported by this package. They are part of the JSON output, so
// they must stay stable.
const (
	CodeNotFound     = "not_found"
	CodeUnauthorized = "unauthorized"
	CodeRateLimited  = "rate_limited"
	CodeAPIError     = "api_error"
)

// Error is a failure to obtain the star history, with a stable code.
type Error struct {
	Code    string
	Message string
	Err     error
}

// Error returns the message, followed by the cause when there is one.
func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}

	return e.Message
}

// Unwrap returns the cause of the error, if any.
func (e *Error) Unwrap() error {
	return e.Err
}
