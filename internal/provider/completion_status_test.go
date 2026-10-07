package provider

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

var _ CompletionStatusClient = (*SMSBower)(nil)

func TestMailTransitionAcknowledgmentsMatchOfficialPostmanExamples(t *testing.T) {
	for _, tc := range []struct {
		name, status, body string
		operation          func(*SMSBower) error
	}{
		{"complete", "3", `{"status":1,"message":"Activation succeed"}`, func(c *SMSBower) error { return c.Complete(context.Background(), "email", "123") }},
		{"complete_compatibility", "3", `{"status":1,"message":"Success"}`, func(c *SMSBower) error { return c.Complete(context.Background(), "email", "123") }},
		{"complete_trim_case", "3", `{"status":"1","message":" ACTIVATION SUCCEED ","error":null}`, func(c *SMSBower) error { return c.Complete(context.Background(), "email", "123") }},
		{"next", "5", `{"status":1,"message":"Wait for next code"}`, func(c *SMSBower) error { return c.NextCode(context.Background(), "email", "123") }},
		{"next_compatibility", "5", `{"status":1,"message":"Success"}`, func(c *SMSBower) error { return c.NextCode(context.Background(), "email", "123") }},
		{"cancel", "2", `{"status":1,"message":"Success"}`, func(c *SMSBower) error { return c.Cancel(context.Background(), "email", "123") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				q := r.URL.Query()
				if r.Method != http.MethodGet || r.URL.Path != "/api/mail/setStatus" || q.Get("id") != "123" || q.Get("status") != tc.status || q.Get("api_key") != testKey || len(q) != 3 {
					t.Error("unexpected transition request")
				}
				fmt.Fprint(w, tc.body)
			})
			if err := tc.operation(client); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatalf("transition repeated %d times", calls.Load())
			}
		})
	}
}

func TestMailTransitionRejectsContradictoryOrWrongOperationAcknowledgments(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		operation     func(*SMSBower) error
		uncertain     bool
	}{
		{"complete", "Activation succeed", func(c *SMSBower) error { return c.Complete(context.Background(), "email", "123") }, false},
		{"cancel", "Success", func(c *SMSBower) error { return c.Cancel(context.Background(), "email", "123") }, false},
		{"next", "Wait for next code", func(c *SMSBower) error { return c.NextCode(context.Background(), "email", "123") }, true},
	} {
		for _, responseError := range []string{`"Bad actual activation status"`, `true`, `{}`, `[]`} {
			t.Run(tc.name+responseError, func(t *testing.T) {
				client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprintf(w, `{"status":1,"message":%q,"error":%s}`, tc.message, responseError)
				})
				typedError(t, tc.operation(client), "invalid_response", tc.uncertain)
			})
		}
	}
	for _, tc := range []struct {
		body      string
		operation func(*SMSBower) error
		uncertain bool
	}{
		{`{"status":1,"message":"Wait for next code"}`, func(c *SMSBower) error { return c.Complete(context.Background(), "email", "123") }, false},
		{`{"status":1,"message":"Activation succeed"}`, func(c *SMSBower) error { return c.Cancel(context.Background(), "email", "123") }, false},
		{`{"status":1,"message":"Activation succeed"}`, func(c *SMSBower) error { return c.NextCode(context.Background(), "email", "123") }, true},
	} {
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) })
		typedError(t, tc.operation(client), "invalid_response", tc.uncertain)
	}
}

func TestCompletionStatusReadsActualStateWithoutClosingOrReturningCode(t *testing.T) {
	for _, tc := range []struct{ state, want string }{
		{"3", "completed"}, {`"3"`, "completed"},
		{"1", "waiting"}, {"2", "waiting"}, {"5", "waiting"}, {"42", "waiting"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			var calls atomic.Int32
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				q := r.URL.Query()
				if r.Method != http.MethodGet || r.URL.Path != "/api/mail/getStatus" || q.Get("id") != "123" || q.Get("api_key") != testKey || len(q) != 2 {
					t.Error("completion lookup must be a read-only status request")
				}
				fmt.Fprintf(w, `{"status":1,"data":{"status":%s,"status_description":"Mail has been activated","available_to_get_next_code":false,"last_code":%q}}`, tc.state, testKey)
			})
			result, err := client.CompletionStatus(context.Background(), "email", "123")
			if err != nil || result.Status != tc.want || result.Code != "" || calls.Load() != 1 {
				t.Fatalf("result %s, code returned %v, calls %d, error %v", result.Status, result.Code != "", calls.Load(), err)
			}
		})
	}
}

func TestCompletionStatusCannotInferClosureFromOperationSuccessOrMalformedData(t *testing.T) {
	for _, body := range []string{
		`{"status":1}`, `{"status":3}`, `{"status":1,"code":"123456"}`,
		`{"status":1,"data":null}`, `{"status":1,"data":[]}`,
		`{"status":1,"data":{"status_description":"Mail has been activated"}}`,
		`{"status":1,"data":{"status":true}}`, `{"status":1,"data":{"status":3.5}}`,
		`{"status":1,"data":{"status":-1}}`, `{"status":1,"data":{"status":9999999999}}`,
		`{"status":1,"data":{"status":"03"}}`, `{"status":1,"data":{"status":"completed"}}`,
		`{"status":1,"error":"Bad actual activation status","data":{"status":3}}`,
		`{"status":1,"error":{},"data":{"status":3}}`,
		`{"status":1,"data":{"status":3,"error":"unknown"}}`,
		`{"status":0,"data":{"status":3}}`, `{"status":1,`,
	} {
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		result, err := client.CompletionStatus(context.Background(), "email", "123")
		typedError(t, err, "invalid_response", false)
		if result.Status != "" || result.Code != "" {
			t.Fatal("malformed response returned state evidence")
		}
	}
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":0,"error":"No activation found with such id"}`)
	})
	_, err := client.CompletionStatus(context.Background(), "email", "123")
	typedError(t, err, "activation_not_found", false)
}

func TestCompletionStatusCannotLeakCredentialsOrRedirect(t *testing.T) {
	for _, tc := range []struct{ name, code string }{{"unknown", "invalid_response"}, {"redirect", "upstream_http"}, {"timeout", "upstream_unavailable"}} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch tc.name {
				case "unknown":
					fmt.Fprintf(w, `{"status":0,"error":%q}`, "https://private.test/?api_key="+testKey)
				case "redirect":
					http.Redirect(w, r, "/redirected?api_key="+testKey, http.StatusTemporaryRedirect)
				case "timeout":
					<-r.Context().Done()
				}
			})
			client.client.Timeout = 100 * time.Millisecond
			_, err := client.CompletionStatus(context.Background(), "email", "123")
			typedError(t, err, tc.code, false)
			if calls.Load() != 1 {
				t.Fatalf("read-only status issued %d calls", calls.Load())
			}
		})
	}
}

func TestCompletionStatusRejectsOtherKindsAndInvalidIDsLocally(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid lookup contacted upstream") })
	for _, tc := range []struct{ kind, id string }{{"phone", "123"}, {"other", "123"}, {"email", ""}, {"email", "123&status=2"}} {
		_, err := client.CompletionStatus(context.Background(), tc.kind, tc.id)
		typedError(t, err, "invalid_request", false)
	}
}
