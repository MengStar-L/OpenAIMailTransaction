package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBalanceUsesOfficialReadOnlyEndpointAndPreservesDecimals(t *testing.T) {
	for _, amount := range []string{"0", "0.0000", "2.1419", "0.00000001", "999999999.12345678"} {
		t.Run(amount, func(t *testing.T) {
			var calls atomic.Int32
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				q := r.URL.Query()
				if r.Method != http.MethodGet || r.URL.Path != "/stubs/handler_api.php" || len(q) != 2 || q.Get("action") != "getBalance" || q.Get("api_key") != testKey {
					t.Error("unexpected balance request")
				}
				fmt.Fprint(w, "ACCESS_BALANCE:"+amount+"\r\n")
			})
			got, err := client.Balance(context.Background())
			if err != nil || got.Amount != amount || calls.Load() != 1 {
				t.Fatalf("balance = %+v error=%v calls=%d", got, err, calls.Load())
			}
		})
	}
}

func TestBalanceRejectsMalformedAndSecretBearingResponses(t *testing.T) {
	for _, body := range []string{"", "0", "ACCESS_BALANCE:", "ACCESS_BALANCE: 1", "ACCESS_BALANCE:-1", "ACCESS_BALANCE:+1", "ACCESS_BALANCE:01", "ACCESS_BALANCE:NaN", "ACCESS_BALANCE:1e2", "ACCESS_BALANCE:1/2", "ACCESS_BALANCE:1.000000001", "ACCESS_BALANCE:1000000000", "ACCESS_BALANCE:1\nACCESS_BALANCE:2", `{"balance":1}`, "ACCESS_BALANCE:" + testKey, "<html>" + testKey + "</html>"} {
		t.Run(fmt.Sprintf("case_%x", body), func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
			balance, err := client.Balance(context.Background())
			typedError(t, err, "invalid_response", false)
			if balance.Amount != "" || strings.Contains(err.Error(), testKey) {
				t.Fatal("invalid response leaked data or claimed a balance")
			}
		})
	}
}

func TestBalanceErrorsAreSanitizedAndNeverUncertain(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "BAD_KEY") })
	_, err := client.Balance(context.Background())
	typedError(t, err, "bad_key", false)
	client = testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, testKey)
	})
	_, err = client.Balance(context.Background())
	typedError(t, err, "upstream_http", false)
	if strings.Contains(err.Error(), testKey) {
		t.Fatal("HTTP error leaked API key")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Balance(ctx)
	typedError(t, err, "upstream_unavailable", false)
	if strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), "api_key") {
		t.Fatal("transport error leaked credential URL")
	}
}

func TestDemoBalanceIsDeterministicAndHonorsCancellation(t *testing.T) {
	client := NewDemo().(BalanceClient)
	got, err := client.Balance(context.Background())
	if err != nil || got.Amount != "100.00" {
		t.Fatalf("demo balance = %+v, %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Balance(ctx)
	typedError(t, err, "interrupted", false)
}
