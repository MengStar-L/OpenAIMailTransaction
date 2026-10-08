package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "secret-test-api-key-do-not-expose"

func testClient(t *testing.T, handler http.HandlerFunc) *SMSBower {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewSMSBower(server.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func phoneRequest() Request {
	return Request{Kind: "phone", Service: "dr", Country: "187", MaxPrice: "0.2500", TTL: 20 * time.Minute}
}

func emailRequest() Request {
	return Request{Kind: "email", Service: "dr", Domain: "gmail.com", MaxPrice: "0.1500", TTL: 15 * time.Minute}
}

func typedError(t *testing.T, err error, code string, uncertain bool) *Error {
	t.Helper()
	var got *Error
	if !errors.As(err, &got) || got.Code != code || got.Uncertain != uncertain {
		t.Fatalf("error = %#v, want %s (uncertain %v)", err, code, uncertain)
	}
	if strings.Contains(got.Error(), testKey) || strings.Contains(got.Error(), "http") || strings.Contains(got.Error(), "api_key") {
		t.Fatalf("credential/URL leaked: %s", got.Error())
	}
	return got
}

func TestAllocatePhoneV2PreservesPriceAndUpstreamDeadline(t *testing.T) {
	deadline := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/stubs/handler_api.php" || q.Get("action") != "getNumberV2" || q.Get("service") != "dr" || q.Get("country") != "187" || q.Get("maxPrice") != "0.2500" || q.Get("api_key") != testKey {
			t.Errorf("unexpected phone request shape")
		}
		fmt.Fprintf(w, `{"activationId":12345,"phoneNumber":"447700900123","activationCost":0.1,"activationTime":"2000-01-01T00:00:00Z","expiresAt":%q}`, deadline.Format(time.RFC3339))
	})
	before := time.Now()
	activation, err := client.Allocate(context.Background(), phoneRequest())
	if err != nil {
		t.Fatal(err)
	}
	if activation.ID != "12345" || activation.Resource != "+447700900123" || !activation.ExpiresAt.Equal(deadline) || activation.CancelAfter.Before(before) || activation.CancelAfter.After(time.Now()) {
		t.Fatalf("incorrect activation: %+v", activation)
	}
}

func TestNewPhoneCanImmediatelyRequestUpstreamCancellation(t *testing.T) {
	for _, allocationBody := range []string{
		`{"activationId":778899,"phoneNumber":"447700900123"}`,
		"ACCESS_NUMBER:778899:447700900123",
	} {
		t.Run(allocationBody, func(t *testing.T) {
			var allocations, cancellations atomic.Int32
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if r.URL.Path != "/stubs/handler_api.php" || q.Get("api_key") != testKey {
					t.Error("unexpected phone lifecycle request")
				}
				switch q.Get("action") {
				case "getNumberV2":
					allocations.Add(1)
					fmt.Fprint(w, allocationBody)
				case "setStatus":
					cancellations.Add(1)
					if q.Get("id") != "778899" || q.Get("status") != "8" {
						t.Error("cancellation must target the newly allocated phone")
					}
					fmt.Fprint(w, "ACCESS_CANCEL")
				default:
					t.Error("unexpected request in allocate/cancel lifecycle")
				}
			})
			activation, err := client.Allocate(context.Background(), phoneRequest())
			if err != nil {
				t.Fatal(err)
			}
			if activation.CancelAfter.After(time.Now()) {
				t.Fatalf("allocation imposed an unreported cancellation delay: %v", activation.CancelAfter)
			}
			if err := client.Cancel(context.Background(), "phone", activation.ID); err != nil {
				t.Fatalf("immediate upstream cancellation rejected locally: %v", err)
			}
			if allocations.Load() != 1 || cancellations.Load() != 1 {
				t.Fatalf("unexpected request counts: allocations=%d cancellations=%d", allocations.Load(), cancellations.Load())
			}
		})
	}
}

func TestEarlyCancellationRefusalHasShortLocalRetryThrottle(t *testing.T) {
	var cancellations atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("action") != "setStatus" || q.Get("id") != "123" || q.Get("status") != "8" {
			t.Error("wrong phone cancellation request")
		}
		if cancellations.Add(1) <= 2 {
			fmt.Fprint(w, "EARLY_CANCEL_DENIED")
			return
		}
		fmt.Fprint(w, "STATUS_CANCEL")
	})
	for attempt := 1; attempt <= 2; attempt++ {
		e := typedError(t, client.Cancel(context.Background(), "phone", "123"), "early_cancel", false)
		if e.RetryAfter != 10*time.Second {
			t.Fatalf("retry throttle = %v, want 10 seconds", e.RetryAfter)
		}
		if cancellations.Load() != int32(attempt) {
			t.Fatal("the provider must not internally retry a rejected cancellation")
		}
	}
	if err := client.Cancel(context.Background(), "phone", "123"); err != nil {
		t.Fatalf("later explicit cancellation confirmation was not accepted: %v", err)
	}
}

func TestAllocatePhoneLegacyResponseDoesNotIssueAnotherRequest(t *testing.T) {
	var requests atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, "ACCESS_NUMBER:778899:447700900123")
	})
	activation, err := client.Allocate(context.Background(), phoneRequest())
	if err != nil || activation.ID != "778899" || requests.Load() != 1 {
		t.Fatalf("allocation %v, %v; request count %d", activation, err, requests.Load())
	}
}

func TestAllocateMailPriceDomainAndNumericID(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/api/mail/getActivation" || q.Get("service") != "dr" || q.Get("domain") != "gmail.com" || q.Get("maxPrice") != "0.1500" || q.Get("alias") != "0" {
			t.Error("unexpected mail request shape")
		}
		fmt.Fprint(w, `{"status":1,"mail":"sample@example.com","mailId":9007199254740993}`)
	})
	before := time.Now()
	activation, err := client.Allocate(context.Background(), emailRequest())
	if err != nil || activation.ID != "9007199254740993" || activation.Resource != "sample@example.com" {
		t.Fatalf("activation = %+v, error = %v", activation, err)
	}
	if activation.ExpiresAt.Before(before.Add(14*time.Minute)) || activation.ExpiresAt.After(time.Now().Add(15*time.Minute)) {
		t.Fatal("missing upstream expiry must use configured local deadline")
	}
}

func TestAllocationErrorsAreConservativeAndSanitized(t *testing.T) {
	cases := []struct {
		name, body, code string
		kind             string
		uncertain        bool
	}{
		{"phone_no_stock", "NO_NUMBERS", "no_stock", "phone", false},
		{"phone_bad_key", "BAD_KEY", "bad_key", "phone", false},
		{"mail_no_stock", `{"status":0,"error":"No mails yet"}`, "no_stock", "email", false},
		{"mail_no_balance", `{"status":0,"error":"Insufficient balance"}`, "no_balance", "email", false},
		{"mail_bad_domain", `{"status":0,"error":"No such domain"}`, "upstream_configuration", "email", false},
		{"mail_price_unavailable", `{"status":0,"error":"Not available at this price. Try another","data":{"actual_available_price":2.5}}`, "price_unavailable", "email", false},
		{"mail_price_with_id", `{"status":0,"error":"Not available at this price. Try another","mailId":44}`, "invalid_response", "email", true},
		{"mail_price_with_address", `{"status":0,"error":"Not available at this price. Try another","mail":"sample@example.com"}`, "invalid_response", "email", true},
		{"mail_price_with_success_status", `{"status":1,"error":"Not available at this price. Try another"}`, "invalid_response", "email", true},
		{"mail_price_without_status", `{"error":"Not available at this price. Try another"}`, "invalid_response", "email", true},
		{"mail_price_plain_text", `Not available at this price. Try another`, "invalid_response", "email", true},
		{"phone_price_mail_error", `{"status":0,"error":"Not available at this price. Try another"}`, "invalid_response", "phone", true},
		{"truncated_allocation", `{"activationId":"123",`, "invalid_response", "phone", true},
		{"unknown_error", `{"status":0,"error":"` + testKey + ` https://private.test/?api_key=secret"}`, "invalid_response", "email", true},
		{"html", "<html>" + testKey + "</html>", "invalid_response", "phone", true},
		{"missing_id", `{"status":1,"mail":"sample@example.com"}`, "invalid_response", "email", true},
		{"display_mail", `{"status":1,"mail":"Some Name <sample@example.com>","mailId":44}`, "invalid_response", "email", true},
		{"empty_phone", `{"activationId":44,"phoneNumber":""}`, "invalid_response", "phone", true},
		{"array", `[1,2,3]`, "invalid_response", "phone", true},
		{"contradictory_phone", `{"error":"NO_NUMBERS","activationId":44,"phoneNumber":"447700900123"}`, "invalid_response", "phone", true},
		{"contradictory_mail", `{"status":0,"error":"No mails yet","mail":"sample@example.com","mailId":44}`, "invalid_response", "email", true},
		{"two_json_objects", `{"status":1,"mail":"sample@example.com","mailId":44}{}`, "invalid_response", "email", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) })
			req := phoneRequest()
			if tc.kind == "email" {
				req = emailRequest()
			}
			_, err := client.Allocate(context.Background(), req)
			typedError(t, err, tc.code, tc.uncertain)
		})
	}
}

func TestMailPriceRefusalDoesNotExposeMetadataOrChangePurchase(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		if q.Get("maxPrice") != "0.1500" || q.Get("alias") != "0" || q.Get("service") != "dr" || q.Get("domain") != "gmail.com" {
			t.Error("price refusal must not change purchase conditions")
		}
		fmt.Fprintf(w, `{"status":0,"error":"Not available at this price. Try another","data":{"actual_available_price":123456.789,"debug":%q}}`, testKey+" https://private.test/?api_key=secret")
	})
	_, err := client.Allocate(context.Background(), emailRequest())
	got := typedError(t, err, "price_unavailable", false)
	if got.Message != "当前价格上限下暂无可分配邮箱，请检查价格设置" {
		t.Fatalf("unexpected public message: %s", got.Message)
	}
	encoded, marshalErr := json.Marshal(got)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, private := range []string{testKey, "http", "api_key", "actual_available_price", "123456.789", "Not available at this price"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("upstream data leaked in serialized error: %s", encoded)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("price refusal made %d purchase requests", calls.Load())
	}
}

func TestAllocateHTTPTransportAndLargeResponsesAreUncertain(t *testing.T) {
	t.Run("http_status", func(t *testing.T) {
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, "BAD_KEY "+testKey)
		})
		_, err := client.Allocate(context.Background(), phoneRequest())
		if typedError(t, err, "upstream_http", true).RetryAfter != 5*time.Second {
			t.Fatal("missing Retry-After")
		}
	})
	t.Run("closed_server", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		client, _ := NewSMSBower(server.URL, testKey)
		server.Close()
		_, err := client.Allocate(context.Background(), phoneRequest())
		typedError(t, err, "upstream_unavailable", true)
	})
	t.Run("body_limit", func(t *testing.T) {
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat("x", maxResponseBytes+1)) })
		_, err := client.Allocate(context.Background(), phoneRequest())
		typedError(t, err, "invalid_response", true)
	})
}

func TestNoRedirectOrAutomaticRetryOnAllocation(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "/redirected?api_key="+url.QueryEscape(testKey), http.StatusTemporaryRedirect)
	})
	_, err := client.Allocate(context.Background(), phoneRequest())
	typedError(t, err, "upstream_http", true)
	if calls.Load() != 1 {
		t.Fatalf("allocation made %d requests", calls.Load())
	}
}

func TestPurchasesCannotUseTransparentPooledConnectionRetry(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !r.Close {
			t.Error("state-changing GET must not reuse a pooled connection")
		}
		fmt.Fprint(w, "ACCESS_NUMBER:778899:447700900123")
	})
	for range 2 {
		if _, err := client.Allocate(context.Background(), phoneRequest()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInvalidConfigNeverCallsUpstream(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid configuration made a purchase") })
	for _, change := range []func(*Request){
		func(r *Request) { r.MaxPrice = "" },
		func(r *Request) { r.MaxPrice = "0" },
		func(r *Request) { r.MaxPrice = "NaN" },
		func(r *Request) { r.MaxPrice = "1e9" },
		func(r *Request) { r.Country = "" },
		func(r *Request) { r.Service = "" },
		func(r *Request) { r.TTL = 0 },
	} {
		req := phoneRequest()
		change(&req)
		_, err := client.Allocate(context.Background(), req)
		typedError(t, err, "invalid_request", false)
	}
	for _, base := range []string{"http://example.com", "https://user:secret@example.com", "https://example.com/path", "https://example.com?api_key=secret", "https://example.com#fragment"} {
		if _, err := NewSMSBower(base, testKey); err == nil {
			t.Errorf("accepted invalid base URL %q", base)
		}
	}
}

func TestPollDocumentedResponses(t *testing.T) {
	cases := []struct{ kind, body, status, code string }{
		{"phone", "STATUS_WAIT_CODE", "waiting", ""},
		{"phone", "STATUS_OK:001234", "received", "001234"},
		{"phone", "STATUS_WAIT_RETRY:001234", "received", "001234"},
		{"phone", "STATUS_CANCEL", "cancelled", ""},
		{"email", `{"status":0,"error":"Code has not been received yet, please try again later"}`, "waiting", ""},
		{"email", `{"status":1,"code":"001234"}`, "received", "001234"},
		{"email", `{"status":0,"error":"Activation is already canceled"}`, "cancelled", ""},
	}
	for _, tc := range cases {
		t.Run(tc.kind+tc.status+tc.code, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.kind == "phone" && (r.URL.Query().Get("action") != "getStatus" || r.URL.Query().Get("id") != "123") {
					t.Error("bad phone status request")
				}
				if tc.kind == "email" && (r.URL.Path != "/api/mail/getCode" || r.URL.Query().Get("mailId") != "123") {
					t.Error("bad email status request")
				}
				fmt.Fprint(w, tc.body)
			})
			result, err := client.Poll(context.Background(), tc.kind, "123")
			if err != nil || result.Status != tc.status || result.Code != tc.code {
				t.Fatalf("result = %+v; error = %v", result, err)
			}
		})
	}
}

func TestPollUnknownOrMissingActivationNeverMeansReleased(t *testing.T) {
	for _, body := range []string{"NO_ACTIVATION", "STATUS_OK:", "STATUS_UNKNOWN", "STATUS_OK:" + testKey} {
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		result, err := client.Poll(context.Background(), "phone", "123")
		if err == nil || result.Status == "cancelled" || result.Status == "expired" {
			t.Fatalf("unsafe phone result %+v; error = %v", result, err)
		}
	}
	for _, body := range []string{`{"status":1}`, `{"status":0,"error":"Pass mail id"}`, `{"status":0,"error":"No activation found with such id"}`, `{"status":1,"code":"https://example.com"}`} {
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		result, err := client.Poll(context.Background(), "email", "123")
		if err == nil || result.Status == "cancelled" || result.Status == "expired" {
			t.Fatalf("unsafe email result %+v; error = %v", result, err)
		}
	}
}

func TestCancelAndCompleteUseDistinctDocumentedStatuses(t *testing.T) {
	for _, tc := range []struct {
		kind, status, body string
		cancel             bool
	}{
		{"phone", "8", "ACCESS_CANCEL", true},
		{"phone", "6", "ACCESS_ACTIVATION", false},
		{"email", "2", `{"status":1,"message":"Success"}`, true},
		{"email", "3", `{"status":1,"message":"Success"}`, false},
	} {
		t.Run(tc.kind+tc.status, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("status") != tc.status || r.URL.Query().Get("id") != "123" {
					t.Error("wrong transition request")
				}
				fmt.Fprint(w, tc.body)
			})
			var err error
			if tc.cancel {
				err = client.Cancel(context.Background(), tc.kind, "123")
			} else {
				err = client.Complete(context.Background(), tc.kind, "123")
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUnconfirmedCancellationCannotReleaseResource(t *testing.T) {
	for _, tc := range []struct{ kind, body string }{
		{"phone", "NO_ACTIVATION"}, {"phone", "ACCESS_ACTIVATION"}, {"phone", "ACCESS_READY"},
		{"phone", "STATUS_WAIT_CODE"}, {"phone", "STATUS_UNKNOWN"}, {"phone", "ACCESS_CANCEL:" + testKey},
		{"email", `{"status":1}`}, {"email", `{"status":0,"error":"Bad actual activation status"}`},
		{"email", `{"status":0,"error":"No activation found with such id"}`},
	} {
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) })
		if err := client.Cancel(context.Background(), tc.kind, "123"); err == nil {
			t.Fatalf("unconfirmed cancellation accepted: %s", tc.body)
		}
	}
}

func TestCancellationTransportFailureDoesNotConfirmRelease(t *testing.T) {
	t.Run("upstream_throttle", func(t *testing.T) {
		var requests atomic.Int32
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, "ACCESS_CANCEL")
		})
		e := typedError(t, client.Cancel(context.Background(), "phone", "123"), "upstream_http", false)
		if e.RetryAfter != time.Minute || requests.Load() != 1 {
			t.Fatalf("upstream throttle was not preserved: delay=%v requests=%d", e.RetryAfter, requests.Load())
		}
	})
	t.Run("connection_failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		client, err := NewSMSBower(server.URL, testKey)
		if err != nil {
			t.Fatal(err)
		}
		server.Close()
		typedError(t, client.Cancel(context.Background(), "phone", "123"), "upstream_unavailable", false)
	})
}

func TestExplicitExpiryCannotExtendLocalWindowOrUseCreationDate(t *testing.T) {
	now := time.Now().UTC()
	local := now.Add(15 * time.Minute)
	for _, body := range []string{
		`{"activationTime":"2000-01-01T00:00:00Z"}`,
		fmt.Sprintf(`{"expiresAt":%q}`, local.Add(time.Hour).Format(time.RFC3339)),
		`{"expiresAt":"not a time"}`,
	} {
		object, _ := decodeObject([]byte(body))
		if !explicitExpiry(object, local).Equal(local) {
			t.Errorf("wrong deadline from %s", body)
		}
	}
	for _, epoch := range []int64{now.Add(time.Minute).Unix(), now.Add(time.Minute).UnixMilli()} {
		object, _ := decodeObject([]byte(fmt.Sprintf(`{"expires_at":%d}`, epoch)))
		got := explicitExpiry(object, local)
		if got.Before(now) || got.After(now.Add(2*time.Minute)) {
			t.Errorf("incorrect epoch expiry %v", got)
		}
	}
}
