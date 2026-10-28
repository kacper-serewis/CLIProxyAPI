package auth

import "strings"

// statusUpstreamOverloaded is the status Anthropic uses for overloaded_error.
const statusUpstreamOverloaded = 529

// isUpstreamOverloadedResultError reports an upstream capacity error. Anthropic sends
// overloaded_error as HTTP 529 or as an SSE error event, which the Claude executor
// reports as 502 with the event JSON as the message. The overload is provider-wide, so
// cooling the credential would only move every chat bound to it to an account without
// their prompt cache.
func isUpstreamOverloadedResultError(err *Error) bool {
	if err == nil {
		return false
	}
	if statusCodeFromResult(err) == statusUpstreamOverloaded {
		return true
	}
	return strings.Contains(err.Message, `"overloaded_error"`)
}
