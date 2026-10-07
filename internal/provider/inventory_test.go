package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEmailInventoryUsesOfficialReadOnlyEndpointAndExactSelection(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != "/api/mail/getPriceRests" || len(q) != 3 || q.Get("api_key") != testKey || q.Get("service") != "dr" || q.Get("domain") != "gmail.com" {
			t.Error("unexpected inventory request shape")
		}
		fmt.Fprint(w, `{"status":1,"data":{"other":{"gmail.com":{"price":0.01,"count":999}},"dr":{"other.com":{"price":0.02,"count":777},"gmail.com":{"price":0.15,"count":42}}}}`)
	})
	req := emailRequest()
	req.TTL = 0 // Inventory does not allocate or depend on an activation deadline.
	got, err := client.EmailInventory(context.Background(), req)
	if err != nil || got.Count != 42 || calls.Load() != 1 {
		t.Fatalf("inventory = %+v, error = %v, calls = %d", got, err, calls.Load())
	}
}

func TestEmailInventoryPriceAndCountFormats(t *testing.T) {
	cases := []struct {
		name, price, count, limit string
		want                      int
	}{
		{"equal_decimal", `"0.15000000"`, `"42"`, "0.1500", 42},
		{"below_ceiling", `0.14999999`, `5`, "0.15", 5},
		{"above_ceiling", `0.15000001`, `500`, "0.15", 0},
		{"zero_stock", `0.1`, `0`, "0.15", 0},
		{"free_price", `0`, `9`, "0.15", 9},
		{"largest_safe_count", `0.1`, `2147483647`, "0.15", 2147483647},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"status":"1","data":{"dr":{"gmail.com":{"price":%s,"count":%s}}}}`, tc.price, tc.count)
			})
			req := emailRequest()
			req.MaxPrice = tc.limit
			got, err := client.EmailInventory(context.Background(), req)
			if err != nil || got.Count != tc.want {
				t.Fatalf("inventory = %+v, %v; want %d", got, err, tc.want)
			}
		})
	}
}

func TestEmailInventoryRejectsMalformedAndUnknownValues(t *testing.T) {
	responses := []string{
		`null`, `[]`, `<html>` + testKey + `</html>`, `{`, `{"status":1}{}`,
		`{"status":1}`, `{"status":1,"data":null}`, `{"status":1,"data":{}}`,
		`{"status":1,"data":{"dr":{}}}`,
		`{"status":1,"data":{"other":{"gmail.com":{"price":0.1,"count":5}}}}`,
		`{"status":1,"data":{"dr":{"other.com":{"price":0.1,"count":5}}}}`,
		`{"status":1,"data":{"dr":{"gmail.com":null}}}`,
		`{"status":1,"data":{"dr":{"gmail.com":{"count":5}}}}`,
		`{"status":1,"data":{"dr":{"gmail.com":{"price":0.1}}}}`,
		`{"status":0,"error":"No mails yet","data":{"dr":{"gmail.com":{"price":0.1,"count":5}}}}`,
		`{"status":1,"error":"BAD_KEY","data":{"dr":{"gmail.com":{"price":0.1,"count":5}}}}`,
		`{"status":2,"data":{"dr":{"gmail.com":{"price":0.1,"count":5}}}}`,
		`{"status":0,"error":"` + testKey + ` https://private.example/?api_key=secret"}`,
	}
	for _, value := range []string{`-1`, `2147483648`, `99999999999999999999999`, `1.2`, `true`, `null`, `[]`, `{}`, `"-1"`, `" 1 "`, `"+1"`, `"01"`, `"1e2"`, `1e2`, `""`} {
		responses = append(responses, fmt.Sprintf(`{"status":1,"data":{"dr":{"gmail.com":{"price":0.1,"count":%s}}}}`, value))
	}
	for _, value := range []string{`-1`, `1000000000`, `null`, `true`, `[]`, `"NaN"`, `"Infinity"`, `"1/10"`, `"1e-1"`, `""`} {
		responses = append(responses, fmt.Sprintf(`{"status":1,"data":{"dr":{"gmail.com":{"price":%s,"count":5}}}}`, value))
	}
	for i, body := range responses {
		t.Run(fmt.Sprintf("case_%02d", i), func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
			_, err := client.EmailInventory(context.Background(), emailRequest())
			typedError(t, err, "invalid_response", false)
		})
	}
}

func TestEmailInventoryKnownErrorsAndConfirmedEmptyStock(t *testing.T) {
	for _, body := range []string{`{"status":0,"error":"No mails yet"}`, `NO_NUMBERS`} {
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		got, err := client.EmailInventory(context.Background(), emailRequest())
		if err != nil || got.Count != 0 {
			t.Fatalf("confirmed empty stock = %+v %v", got, err)
		}
	}
	for _, tc := range []struct{ body, code string }{
		{`{"status":0,"error":"No such domain"}`, "upstream_configuration"},
		{`{"status":0,"error":"Invalid api key"}`, "bad_key"},
		{`BAD_KEY`, "bad_key"},
		{`{"status":0,"error":"Insufficient balance"}`, "no_balance"},
	} {
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) })
		_, err := client.EmailInventory(context.Background(), emailRequest())
		typedError(t, err, tc.code, false)
	}
}

func TestEmailInventoryInvalidConfigurationNeverContactsProvider(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	cases := []Request{phoneRequest(), {}, {Kind: "email", Service: "dr", Domain: "gmail.com"}}
	for _, value := range []string{"", "0", "-1", "1e2", "1000000000", "garbage"} {
		req := emailRequest()
		req.MaxPrice = value
		cases = append(cases, req)
	}
	for _, value := range []string{"", "gmail.com&service=other", "bad domain", strings.Repeat("x", 254)} {
		req := emailRequest()
		req.Domain = value
		cases = append(cases, req)
	}
	for _, value := range []string{"", "dr&api_key=x", strings.Repeat("x", 129)} {
		req := emailRequest()
		req.Service = value
		cases = append(cases, req)
	}
	for _, req := range cases {
		_, err := client.EmailInventory(context.Background(), req)
		typedError(t, err, "invalid_request", false)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid stock requests contacted upstream %d times", calls.Load())
	}
}

func TestEmailInventoryTransportErrorsAreSanitizedAndReadOnly(t *testing.T) {
	t.Run("http_error", func(t *testing.T) {
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, testKey)
		})
		_, err := client.EmailInventory(context.Background(), emailRequest())
		if typedError(t, err, "upstream_http", false).RetryAfter != 5*time.Second {
			t.Fatal("lost retry delay")
		}
	})
	t.Run("no_redirect", func(t *testing.T) {
		var calls atomic.Int32
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			http.Redirect(w, r, "/secret", http.StatusTemporaryRedirect)
		})
		_, err := client.EmailInventory(context.Background(), emailRequest())
		typedError(t, err, "upstream_http", false)
		if calls.Load() != 1 {
			t.Fatal("followed credential-bearing redirect")
		}
	})
	t.Run("response_limit", func(t *testing.T) {
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat("x", maxResponseBytes+1)) })
		_, err := client.EmailInventory(context.Background(), emailRequest())
		typedError(t, err, "invalid_response", false)
	})
	t.Run("closed_server", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		client, _ := NewSMSBower(server.URL, testKey)
		server.Close()
		_, err := client.EmailInventory(context.Background(), emailRequest())
		typedError(t, err, "upstream_unavailable", false)
	})
}

func TestDemoInventoryIsExplicitlySimulatedAndSurvivesRestart(t *testing.T) {
	for range 2 {
		client, ok := NewDemo().(InventoryClient)
		if !ok {
			t.Fatal("demo has no inventory support")
		}
		got, err := client.EmailInventory(context.Background(), Request{Kind: "email"})
		if err != nil || got.Count != 128 {
			t.Fatalf("demo inventory = %+v %v", got, err)
		}
		_, err = client.EmailInventory(context.Background(), Request{Kind: "phone"})
		typedError(t, err, "invalid_request", false)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = client.EmailInventory(ctx, Request{Kind: "email"})
		typedError(t, err, "interrupted", false)
	}
}
