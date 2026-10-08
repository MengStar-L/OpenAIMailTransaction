package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type diagnosticTransport func(*http.Request) (*http.Response, error)

func (f diagnosticTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type diagnosticBody struct{ cause error }

func (r diagnosticBody) Read([]byte) (int, error) { return 0, r.cause }
func (diagnosticBody) Close() error               { return nil }

func TestTransportDiagnosticsPreservePublicErrorAndUncertainty(t *testing.T) {
	for _, operation := range []string{"allocate", "cancel"} {
		for _, scenario := range []string{"connect_timeout", "request_cancelled", "header_timeout", "body_timeout", "body_cancelled", "body_unknown"} {
			t.Run(operation+"/"+scenario, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
					if scenario == "body_timeout" {
						w.WriteHeader(http.StatusOK)
						w.(http.Flusher).Flush()
					}
					<-r.Context().Done()
				})
				code, reason := "upstream_unavailable", "request_timeout"
				switch scenario {
				case "connect_timeout":
					// A controlled dial timeout includes sensitive diagnostic data.
					// Only Timeout() should survive the provider boundary.
					client.client.Transport.(*http.Transport).DialContext = func(context.Context, string, string) (net.Conn, error) {
						return nil, &url.Error{Op: "dial", URL: "https://private.invalid/?api_key=" + testKey, Err: &net.DNSError{Err: testKey, IsTimeout: true}}
					}
				case "request_cancelled":
					cancel()
					reason = "request_cancelled"
				case "header_timeout":
					client.client.Timeout = 100 * time.Millisecond
				case "body_timeout":
					client.client.Timeout = 100 * time.Millisecond
					code = "invalid_response"
				case "body_cancelled", "body_unknown":
					code, reason = "invalid_response", "request_cancelled"
					cause := fmt.Errorf("https://private.invalid/?api_key=%s: %w", testKey, context.Canceled)
					if scenario == "body_unknown" {
						cause = fmt.Errorf("https://private.invalid/?api_key=%s: %w", testKey, io.ErrUnexpectedEOF)
						reason = ""
					}
					client.client.Transport = diagnosticTransport(func(r *http.Request) (*http.Response, error) {
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: diagnosticBody{cause}, Request: r}, nil
					})
				}
				var err error
				if operation == "allocate" {
					_, err = client.Allocate(ctx, phoneRequest())
				} else {
					err = client.Cancel(ctx, "phone", "123")
				}
				failure := typedError(t, err, code, operation == "allocate")
				if failure.DiagnosticReason != reason {
					t.Fatalf("diagnostic = %q, want %q", failure.DiagnosticReason, reason)
				}
				encoded, marshalErr := json.Marshal(failure)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				for _, private := range []string{testKey, "api_key", "private.invalid", "https://", "DiagnosticReason", "request_timeout", "request_cancelled"} {
					if strings.Contains(string(encoded), private) {
						t.Fatalf("internal transport details leaked: %s", encoded)
					}
				}
			})
		}
	}
}

func TestTransportDiagnosticsClassifyWrappedContextAndFallback(t *testing.T) {
	for _, cause := range []struct {
		err    error
		reason string
	}{
		{fmt.Errorf("private cause: %w", context.Canceled), "request_cancelled"},
		{fmt.Errorf("private cause: %w", context.DeadlineExceeded), "request_timeout"},
		{io.ErrUnexpectedEOF, ""},
	} {
		if got := transportDiagnosticReason(context.Background(), cause.err); got != cause.reason {
			t.Fatalf("diagnostic = %q, want %q", got, cause.reason)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := transportDiagnosticReason(ctx, io.ErrUnexpectedEOF); got != "request_cancelled" {
		t.Fatalf("cancelled context fallback = %q", got)
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if got := transportDiagnosticReason(ctx, io.ErrUnexpectedEOF); got != "request_timeout" {
		t.Fatalf("expired context fallback = %q", got)
	}
}
