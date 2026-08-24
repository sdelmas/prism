package providers

import (
	"os"
	"strconv"
	"time"
)

// RequestTimeoutEnv names the environment variable that overrides a provider's
// HTTP client timeout.
const RequestTimeoutEnv = "PRISM_REQUEST_TIMEOUT_SECONDS"

// requestTimeout returns the HTTP client timeout for a provider, honouring
// PRISM_REQUEST_TIMEOUT_SECONDS when it is set to a positive integer.
//
// The default is per-provider and stays what it was. It needs to be overridable
// because the timeout that fits a non-reasoning model is not the one a
// reasoning model needs: the answer only starts arriving after the thinking
// finishes, so the whole response lands in one late burst and a fixed 120s
// deadline expires while the request is still healthy.
func requestTimeout(fallback time.Duration) time.Duration {
	raw := os.Getenv(RequestTimeoutEnv)
	if raw == "" {
		return fallback
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}
