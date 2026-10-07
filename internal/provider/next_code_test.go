package provider

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

var _ NextCodeClient = (*SMSBower)(nil)
var _ NextCodeClient = (*Demo)(nil)
var _ RoundPollClient = (*Demo)(nil)

func TestNextMailCodeUsesStatusFiveOnceAndPreservesActivation(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != "/api/mail/setStatus" || q.Get("id") != "123" || q.Get("status") != "5" || q.Get("api_key") != testKey || len(q) != 3 {
			t.Error("unexpected next-code request shape")
		}
		if !r.Close {
			t.Error("next-code transition must not reuse pooled connections")
		}
		fmt.Fprint(w, `{"status":1,"message":"Success"}`)
	})
	if err := client.NextCode(context.Background(), "email", "123"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("transition issued %d calls", calls.Load())
	}
}

func TestNextMailCodeDistinguishesRejectionFromUncertainAcknowledgment(t *testing.T) {
	cases := []struct {
		name, body, code string
		uncertain        bool
	}{
		{"known_rejection", `{"status":0,"error":"Bad actual activation status"}`, "bad_status", false},
		{"missing_activation", `{"status":0,"error":"No activation found with such id"}`, "activation_not_found", false},
		{"bad_key", `{"status":0,"error":"Invalid API key"}`, "bad_key", false},
		{"unknown_error", `{"status":0,"error":"` + testKey + ` https://private.test/?api_key=secret"}`, "invalid_response", true},
		{"missing_acknowledgment", `{"status":1}`, "invalid_response", true},
		{"contradictory_acknowledgment", `{"status":1,"message":"Success","error":"Bad actual activation status"}`, "invalid_response", true},
		{"truncated_response", `{"status":1,`, "invalid_response", true},
		{"already_cancelled", `{"status":0,"error":"Activation is already canceled"}`, "invalid_response", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				fmt.Fprint(w, tc.body)
			})
			typedError(t, client.NextCode(context.Background(), "email", "123"), tc.code, tc.uncertain)
			if calls.Load() != 1 {
				t.Fatalf("transition was retried: %d requests", calls.Load())
			}
		})
	}
}

func TestNextMailCodeTimeoutAndRedirectNeverRetryOrLeakCredentials(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		var calls atomic.Int32
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			<-r.Context().Done()
		})
		client.client.Timeout = 100 * time.Millisecond
		typedError(t, client.NextCode(context.Background(), "email", "123"), "upstream_unavailable", true)
		if calls.Load() != 1 {
			t.Fatalf("transition was retried: %d requests", calls.Load())
		}
	})
	t.Run("redirect", func(t *testing.T) {
		var calls atomic.Int32
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			http.Redirect(w, r, "/redirected?api_key="+testKey, http.StatusTemporaryRedirect)
		})
		typedError(t, client.NextCode(context.Background(), "email", "123"), "upstream_http", true)
		if calls.Load() != 1 {
			t.Fatalf("transition followed redirect: %d requests", calls.Load())
		}
	})
}

func TestNextMailCodeRejectsOtherKindsAndBadIDsWithoutUpstream(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid next-code transition contacted upstream")
	})
	for _, tc := range []struct{ kind, id string }{{"phone", "123"}, {"other", "123"}, {"email", ""}, {"email", "123&status=2"}} {
		typedError(t, client.NextCode(context.Background(), tc.kind, tc.id), "invalid_request", false)
	}
}

func TestDemoMailReceiptRoundsSurviveRestartAndStopAtThree(t *testing.T) {
	id := demoID("email", 46*time.Second, 15*time.Minute)
	seen := map[string]bool{}
	for round := 1; round <= 3; round++ {
		client := NewDemo().(RoundPollClient)
		result, err := client.PollRound(context.Background(), "email", id, round)
		if err != nil || result.Status != "received" || result.Code == "" || seen[result.Code] {
			t.Fatalf("round %d: result %+v, error %v", round, result, err)
		}
		seen[result.Code] = true
		resumed, err := NewDemo().(RoundPollClient).PollRound(context.Background(), "email", id, round)
		if err != nil || resumed != result {
			t.Fatalf("restart changed round %d: %+v versus %+v (%v)", round, resumed, result, err)
		}
	}
	if err := NewDemo().(NextCodeClient).NextCode(context.Background(), "email", id); err != nil {
		t.Fatal(err)
	}
	for _, round := range []int{0, 4} {
		_, err := NewDemo().(RoundPollClient).PollRound(context.Background(), "email", id, round)
		typedError(t, err, "invalid_request", false)
	}
}

func TestDemoLaterRoundKeepsWaitingAndCannotReviveExpiredMailbox(t *testing.T) {
	id := demoID("email", 16*time.Second, 15*time.Minute)
	result, err := NewDemo().(RoundPollClient).PollRound(context.Background(), "email", id, 2)
	if err != nil || result.Status != "waiting" || result.Code != "" {
		t.Fatalf("second round arrived too soon: %+v %v", result, err)
	}
	expired := demoID("email", 26*time.Second, 25*time.Second)
	result, err = NewDemo().(RoundPollClient).PollRound(context.Background(), "email", expired, 2)
	if err != nil || result.Status != "expired" {
		t.Fatalf("expired mailbox produced another code: %+v %v", result, err)
	}
	typedError(t, NewDemo().(NextCodeClient).NextCode(context.Background(), "email", expired), "bad_status", false)
	typedError(t, NewDemo().(NextCodeClient).NextCode(context.Background(), "phone", demoID("phone", 16*time.Second, time.Hour)), "invalid_request", false)
}
