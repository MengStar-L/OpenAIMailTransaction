package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"openai-mail-transaction/internal/provider"
)

func TestAllocationUncertainAuditOnlyIncludesSafeDiagnosticLabels(t *testing.T) {
	secret := "https://provider.invalid/?api_key=PRIVATE-CREDENTIAL"
	for _, tc := range []struct {
		name, reason string
		err          error
	}{
		{"cancelled", "request_cancelled", fmt.Errorf("%s: %w", secret, context.Canceled)},
		{"timeout", "request_timeout", fmt.Errorf("%s: %w", secret, context.DeadlineExceeded)},
		{"transport", "upstream_unavailable", &provider.Error{Code: "upstream_unavailable", Message: secret, Uncertain: true}},
		{"http", "upstream_http", &provider.Error{Code: "upstream_http", Message: secret, Uncertain: true}},
		{"body", "invalid_response", &provider.Error{Code: "invalid_response", Message: secret, Uncertain: true}},
		{"provider_timeout", "request_timeout", &provider.Error{Code: "upstream_unavailable", DiagnosticReason: "request_timeout", Message: secret, Uncertain: true}},
		{"provider_cancelled", "request_cancelled", &provider.Error{Code: "invalid_response", DiagnosticReason: "request_cancelled", Message: secret, Uncertain: true}},
		{"unknown_diagnostic", "upstream_unavailable", &provider.Error{Code: "upstream_unavailable", DiagnosticReason: secret, Message: secret, Uncertain: true}},
		{"unknown_code", "unknown_result", &provider.Error{Code: secret, Message: secret, Uncertain: true}},
		{"unknown_error", "unknown_result", errors.New(secret)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detail := allocationUncertainDetail("phone", tc.err, 12345*time.Millisecond)
			var value struct {
				Kind      string `json:"kind"`
				Reason    string `json:"reason"`
				ElapsedMS int64  `json:"elapsed_ms"`
			}
			if err := json.Unmarshal([]byte(detail), &value); err != nil || value.Kind != "phone" || value.Reason != tc.reason || value.ElapsedMS != 12345 || strings.Contains(detail, "PRIVATE") || strings.Contains(detail, "provider.invalid") {
				t.Fatalf("unsafe or incorrect audit: %s", detail)
			}
		})
	}
	if got := allocationUncertainDetail(secret, errors.New(secret), -time.Second); got != `{"kind":"unknown","reason":"unknown_result","elapsed_ms":0}` {
		t.Fatalf("unsafe unrecognized kind: %s", got)
	}
}

func TestUncertainAllocationPersistsDiagnosticAudit(t *testing.T) {
	p := &testProvider{allocationErrors: []error{&provider.Error{Code: "invalid_response", Message: "sensitive upstream response", Uncertain: true}}}
	a := newTestApp(t, p)
	order := redeem(t, a, "DEMO-PHONE")
	var detail string
	if err := a.db.QueryRow("SELECT detail FROM audit WHERE action='allocation_uncertain' AND object_id=?", order.Order.ID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(detail), &value); err != nil || value["kind"] != "phone" || value["reason"] != "invalid_response" || value["elapsed_ms"] == nil || strings.Contains(detail, "sensitive") {
		t.Fatalf("missing/sensitive allocation diagnosis: %s", detail)
	}
}
