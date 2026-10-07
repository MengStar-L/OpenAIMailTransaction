package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"openai-mail-transaction/internal/provider"
)

func savePhonePolicy(t *testing.T, a *App, countries, maxPrice string) {
	t.Helper()
	s := a.currentSettings()
	s.PhoneCountry, s.PhoneMaxPrice = countries, maxPrice
	w := apiRequest(a.Handler(), "PUT", "/api/admin/settings", s, "", adminCookie(t, a), "")
	if w.Code != http.StatusOK {
		t.Fatalf("save policy: %d %s", w.Code, w.Body.String())
	}
}

func phonePolicyCatalog(t *testing.T, a *App, token string) []provider.PhoneChannel {
	t.Helper()
	method, path, body := "POST", "/api/phone/channels", any(map[string]string{"cdk": "DEMO-PHONE"})
	if token != "" {
		method, path, body = "GET", "/api/orders/phone/channels", nil
	}
	w := apiRequest(a.Handler(), method, path, body, token, nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("catalog: %d %s", w.Code, w.Body.String())
	}
	var response struct {
		Channels  []provider.PhoneChannel `json:"channels"`
		Countries string                  `json:"countries"`
		MaxPrice  string                  `json:"max_price"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	current := a.currentSettings()
	if response.Countries != current.PhoneCountry || response.MaxPrice != current.PhoneMaxPrice {
		t.Fatalf("catalog reported stale policy: %s", w.Body.String())
	}
	return response.Channels
}

func TestIssuedPhoneVoucherCatalogFollowsSavedCountryAndPricePolicy(t *testing.T) {
	a, p := catalogTestApp(t)
	var before string
	if err := a.db.QueryRow("SELECT snapshot FROM cdks WHERE hash=?", hash("DEMO-PHONE")).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// Fill the old policy's cache before editing settings.
	phonePolicyCatalog(t, a, "")
	for _, tc := range []struct {
		name, countries, maxPrice string
		providers                 []string
	}{
		{"lower price", "0,187", "0.10", []string{"2368"}},
		{"different countries", "16,187", "0.20", []string{"3243", "9000"}},
		{"all and higher price", "*", "0.30", []string{"2368", "3243", "9000", "4000"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			savePhonePolicy(t, a, tc.countries, tc.maxPrice)
			channels := phonePolicyCatalog(t, a, "")
			var ids []string
			for _, channel := range channels {
				ids = append(ids, channel.ProviderID)
			}
			if !reflect.DeepEqual(ids, tc.providers) {
				t.Fatalf("channels=%v, want %v", ids, tc.providers)
			}
		})
	}
	var after string
	var attempts, used, orders int
	if err := a.db.QueryRow("SELECT snapshot,attempts,used_count FROM cdks WHERE hash=?", hash("DEMO-PHONE")).Scan(&after, &attempts, &used); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow("SELECT COUNT(*) FROM orders").Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if after != before || attempts != 0 || used != 0 || orders != 0 || p.allocations != 0 {
		t.Fatal("policy lookup mutated the voucher or allocated a resource")
	}
}

func TestIssuedPhoneVoucherRejectsOldSelectionsAndCanUseNewPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, countries, maxPrice, oldCountry, oldProvider, newCountry, newProvider, newPrice string
	}{
		{"changed country", "16", "0.20", "0", "2368", "16", "9000", "0.05"},
		{"lowered price", "*", "0.09", "0", "2368", "16", "9000", "0.05"},
		{"raised price", "0", "0.30", "187", "3243", "0", "4000", "0.200001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, p := catalogTestApp(t)
			phonePolicyCatalog(t, a, "")
			savePhonePolicy(t, a, tc.countries, tc.maxPrice)
			stale := apiRequest(a.Handler(), "POST", "/api/redeem", map[string]string{"cdk": "DEMO-PHONE", "phone_country": tc.oldCountry, "phone_provider_id": tc.oldProvider}, "", nil, "")
			if stale.Code != http.StatusConflict || p.allocations != 0 {
				t.Fatalf("stale choice reached allocation: %d %s", stale.Code, stale.Body.String())
			}
			allocated := parseOrder(t, apiRequest(a.Handler(), "POST", "/api/redeem", map[string]string{"cdk": "DEMO-PHONE", "phone_country": tc.newCountry, "phone_provider_id": tc.newProvider}, "", nil, ""))
			if allocated.Order.Status != "waiting" || len(p.requests) != 1 {
				t.Fatalf("new policy could not allocate: %+v", allocated.Order)
			}
			req := p.requests[0]
			if req.Country != tc.newCountry || req.ProviderID != tc.newProvider || req.MaxPrice != tc.newPrice || req.Service != "dr" || req.TTL != 20*time.Minute {
				t.Fatalf("wrong purchase policy/contract: %+v", req)
			}
		})
	}
}

func TestPhoneReplacementUsesLatestSavedPolicy(t *testing.T) {
	a, p := catalogTestApp(t)
	first := parseOrder(t, apiRequest(a.Handler(), "POST", "/api/redeem", map[string]string{"cdk": "DEMO-PHONE", "phone_country": "0", "phone_provider_id": "2368"}, "", nil, ""))
	cancelled := parseOrder(t, apiRequest(a.Handler(), "POST", "/api/orders/cancel", nil, first.Token, nil, ""))
	if cancelled.Order.Status != "cancelled" {
		t.Fatal("first order did not cancel")
	}
	savePhonePolicy(t, a, "16", "0.05")
	channels := phonePolicyCatalog(t, a, first.Token)
	if len(channels) != 1 || channels[0].ProviderID != "9000" {
		t.Fatalf("replacement catalog is stale: %+v", channels)
	}
	stale := apiRequest(a.Handler(), "POST", "/api/orders/replace", map[string]string{"phone_country": "0", "phone_provider_id": "2368"}, first.Token, nil, "")
	if stale.Code != http.StatusConflict || p.allocations != 1 {
		t.Fatalf("stale replacement reached allocation: %d %s", stale.Code, stale.Body.String())
	}
	next := parseOrder(t, apiRequest(a.Handler(), "POST", "/api/orders/replace", map[string]string{"phone_country": "16", "phone_provider_id": "9000"}, first.Token, nil, ""))
	if next.Order.ID == first.Order.ID || p.allocations != 2 || p.requests[1].Country != "16" || p.requests[1].MaxPrice != "0.05" {
		t.Fatalf("replacement did not use new policy: %+v requests=%+v", next.Order, p.requests)
	}
}

func TestCurrentPhonePolicyRepairsOldMissingConfigurationAndPreservesActiveOrder(t *testing.T) {
	a, p := catalogTestApp(t)
	a.opts.Mode = "live"
	old := a.currentSettings()
	old.PhoneCountry, old.PhoneMaxPrice, old.PhoneTTLMinutes = "", "", 17
	data, _ := json.Marshal(old)
	if _, err := a.db.Exec("UPDATE cdks SET snapshot=? WHERE hash=?", string(data), hash("DEMO-PHONE")); err != nil {
		t.Fatal(err)
	}
	savePhonePolicy(t, a, "16", "0.05")
	phonePolicyCatalog(t, a, "")
	first := parseOrder(t, apiRequest(a.Handler(), "POST", "/api/redeem", map[string]string{"cdk": "DEMO-PHONE", "phone_country": "16", "phone_provider_id": "9000"}, "", nil, ""))
	if p.requests[0].TTL != 17*time.Minute {
		t.Fatal("current policy changed the original voucher wait duration")
	}
	// Clearing current configuration prevents future purchases, but must not
	// hide or replace a phone already allocated under the preceding policy.
	savePhonePolicy(t, a, "", "")
	resumed := redeem(t, a, "DEMO-PHONE")
	if resumed.Order.ID != first.Order.ID || resumed.Order.Resource != first.Order.Resource || resumed.Order.ExpiresAt.UnixMilli() != first.Order.ExpiresAt.UnixMilli() || p.allocations != 1 {
		t.Fatalf("settings change affected existing allocation: %+v", resumed.Order)
	}
	lookup := apiRequest(a.Handler(), "POST", "/api/phone/channels", map[string]string{"cdk": "DEMO-PHONE"}, "", nil, "")
	var response struct {
		Resume bool `json:"resume"`
	}
	_ = json.Unmarshal(lookup.Body.Bytes(), &response)
	if lookup.Code != http.StatusOK || !response.Resume {
		t.Fatalf("active order lookup revalidated changed policy: %d %s", lookup.Code, lookup.Body.String())
	}
}

func TestMissingCurrentPhonePolicyCannotUseValidOldSnapshot(t *testing.T) {
	a, p := catalogTestApp(t)
	a.opts.Mode = "live"
	savePhonePolicy(t, a, "", "")
	for _, path := range []string{"/api/phone/channels", "/api/redeem"} {
		body := map[string]string{"cdk": "DEMO-PHONE"}
		if path == "/api/redeem" {
			body["phone_country"], body["phone_provider_id"] = "0", "2368"
		}
		w := apiRequest(a.Handler(), "POST", path, body, "", nil, "")
		if w.Code != http.StatusConflict || p.allocations != 0 {
			t.Fatalf("%s used a stale configuration: %d %s", path, w.Code, w.Body.String())
		}
	}
}

type policyHeldCatalogProvider struct {
	*catalogProvider
	entered, release chan struct{}
	enterOnce        sync.Once
}

func (p *policyHeldCatalogProvider) PhoneChannels(ctx context.Context, request provider.Request) ([]provider.PhoneChannel, error) {
	p.enterOnce.Do(func() { close(p.entered) })
	select {
	case <-p.release:
		return p.catalogProvider.PhoneChannels(ctx, request)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestPhonePolicySaveCannotOvertakeValidatedPurchase(t *testing.T) {
	a, base := catalogTestApp(t)
	cookie := adminCookie(t, a)
	p := &policyHeldCatalogProvider{catalogProvider: base, entered: make(chan struct{}), release: make(chan struct{})}
	a.client = p
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(p.release) }) }
	defer release()
	allocated := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		allocated <- apiRequest(a.Handler(), "POST", "/api/redeem", map[string]string{"cdk": "DEMO-PHONE", "phone_country": "0", "phone_provider_id": "2368"}, "", nil, "")
	}()
	select {
	case <-p.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("purchase did not reach catalog")
	}
	s := a.currentSettings()
	s.PhoneCountry, s.PhoneMaxPrice = "16", "0.05"
	saved := make(chan *httptest.ResponseRecorder, 1)
	go func() { saved <- apiRequest(a.Handler(), "PUT", "/api/admin/settings", s, "", cookie, "") }()
	select {
	case response := <-saved:
		t.Fatalf("settings overtook an in-flight purchase: %d %s", response.Code, response.Body.String())
	case <-time.After(30 * time.Millisecond):
	}
	release()
	var first orderEnvelope
	select {
	case response := <-allocated:
		first = parseOrder(t, response)
	case <-time.After(3 * time.Second):
		t.Fatal("purchase did not finish")
	}
	select {
	case response := <-saved:
		if response.Code != http.StatusOK {
			t.Fatalf("settings save failed: %d %s", response.Code, response.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("settings save did not finish")
	}
	parseOrder(t, apiRequest(a.Handler(), "POST", "/api/orders/cancel", nil, first.Token, nil, ""))
	stale := apiRequest(a.Handler(), "POST", "/api/orders/replace", map[string]string{"phone_country": "0", "phone_provider_id": "2368"}, first.Token, nil, "")
	if stale.Code != http.StatusConflict || base.allocations != 1 {
		t.Fatalf("purchase after saved policy reused old allowance: %d %s", stale.Code, stale.Body.String())
	}
}
