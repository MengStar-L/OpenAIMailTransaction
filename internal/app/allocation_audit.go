package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"openai-mail-transaction/internal/provider"
)

// Record only our own bounded labels. Raw transport errors can contain a
// credential-bearing URL, and upstream messages can contain account data.
func allocationUncertainDetail(kind string, err error, elapsed time.Duration) string {
	if kind != "phone" && kind != "email" {
		kind = "unknown"
	}
	reason := "unknown_result"
	var pe *provider.Error
	switch {
	case errors.Is(err, context.Canceled):
		reason = "request_cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		reason = "request_timeout"
	case errors.As(err, &pe):
		switch pe.Code {
		case "request_failed", "upstream_unavailable", "upstream_http", "invalid_response":
			reason = pe.Code
		}
		switch pe.DiagnosticReason {
		case "request_cancelled", "request_timeout":
			reason = pe.DiagnosticReason
		}
	}
	data, _ := json.Marshal(struct {
		Kind      string `json:"kind"`
		Reason    string `json:"reason"`
		ElapsedMS int64  `json:"elapsed_ms"`
	}{kind, reason, max(elapsed.Milliseconds(), 0)})
	return string(data)
}
