package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"openai-mail-transaction/internal/provider"
)

type catalogProvider struct {
	*testProvider
	channels     []provider.PhoneChannel
	catalogErr   error
	catalogDelay time.Duration
}

func (p *catalogProvider) PhoneCountries(context.Context) ([]provider.PhoneCountry, error) {
	return []provider.PhoneCountry{{ID: "0", Name: "俄罗斯"}, {ID: "187", Name: "美国"}}, nil
}
func (p *catalogProvider) PhoneChannels(context.Context, provider.Request) ([]provider.PhoneChannel, error) {
	if p.catalogDelay > 0 {
		time.Sleep(p.catalogDelay)
	}
	return p.channels, p.catalogErr
}

func catalogTestApp(t *testing.T) (*App, *catalogProvider) {
	t.Helper()
	base := &testProvider{}
	a := newTestApp(t, base)
	p := &catalogProvider{testProvider: base, channels: []provider.PhoneChannel{
		{Country: "0", CountryName: "俄罗斯", ProviderID: "2368", Price: "0.10", Count: 20, Tier: "bronze"},
		{Country: "187", CountryName: "美国", ProviderID: "3243", Price: "0.12", Count: 4, Tier: "gold"},
		{Country: "16", CountryName: "英国", ProviderID: "9000", Price: "0.05", Count: 40, Tier: "unknown"},
		{Country: "0", ProviderID: "4000", Price: "0.200001", Count: 40, Tier: "silver"},
		{Country: "0", ProviderID: "4001", Price: "0.1", Count: 0, Tier: "unknown"},
	}}
	a.client = p
	a.settings.PhoneCountry = "0,187"
	a.settings.PhoneMaxPrice = "0.20"
	b, _ := json.Marshal(a.settings)
	if _, err := a.db.Exec("UPDATE cdks SET snapshot=? WHERE kind='phone'", string(b)); err != nil {
		t.Fatal(err)
	}
	return a, p
}

func TestPhoneCountriesNormalization(t *testing.T) {
	for _, tc := range []struct{ in, want string }{{"0，187;16 0", "0,16,187"}, {"*", "*"}, {" 001 , 0 ", "0,1"}, {"", ""}} {
		got, err := normalizePhoneCountries(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("%q: %q %v", tc.in, got, err)
		}
	}
	for _, raw := range []string{"*,0", "-1", "1.2", "100000", "abc", "，"} {
		if _, err := normalizePhoneCountries(raw); err == nil {
			t.Fatalf("accepted invalid countries %q", raw)
		}
	}
}

func TestPhoneVoucherExpiringDuringQuoteDoesNotPurchase(t *testing.T) {
	a, p := catalogTestApp(t)
	p.catalogDelay = 50 * time.Millisecond
	_, err := a.db.Exec("UPDATE cdks SET expires_at=? WHERE hash=?", time.Now().Add(20*time.Millisecond).UnixMilli(), hash("DEMO-PHONE"))
	if err != nil {
		t.Fatal(err)
	}
	w := apiRequest(a.Handler(), "POST", "/api/redeem", map[string]string{"cdk": "DEMO-PHONE", "phone_country": "0", "phone_provider_id": "2368"}, "", nil, "")
	if w.Code != 410 || p.allocations != 0 {
		t.Fatalf("expired during catalog purchased: %d %s", w.Code, w.Body.String())
	}
}

func TestPhoneCatalogReadOnlySnapshotAndPriceFilter(t *testing.T) {
	a, p := catalogTestApp(t)
	// A later settings edit cannot expand an issued voucher's original scope.
	a.settings.PhoneCountry = "*"
	a.settings.PhoneMaxPrice = "99"
	w := apiRequest(a.Handler(), "POST", "/api/phone/channels", map[string]string{"cdk": "DEMO-PHONE"}, "", nil, "")
	if w.Code != 200 {
		t.Fatalf("catalog %d %s", w.Code, w.Body.String())
	}
	var v struct {
		Channels []provider.PhoneChannel `json:"channels"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if len(v.Channels) != 2 {
		t.Fatalf("filtered channels: %s", w.Body.String())
	}
	var attempts, used, orders int
	_ = a.db.QueryRow("SELECT attempts,used_count FROM cdks WHERE hash=?", hash("DEMO-PHONE")).Scan(&attempts, &used)
	_ = a.db.QueryRow("SELECT COUNT(*) FROM orders").Scan(&orders)
	if attempts != 0 || used != 0 || orders != 0 || p.allocations != 0 {
		t.Fatal("catalog consumed voucher or bought a resource")
	}
}

func TestPhoneSelectionBoundariesAndExactPurchase(t *testing.T) {
	a, p := catalogTestApp(t)
	for _, body := range []map[string]string{
		{"cdk": "DEMO-PHONE"},
		{"cdk": "DEMO-PHONE", "phone_country": "16", "phone_provider_id": "9000"},
		{"cdk": "DEMO-PHONE", "phone_country": "0", "phone_provider_id": "4000"},
		{"cdk": "DEMO-PHONE", "phone_country": "0", "phone_provider_id": "4001"},
		{"cdk": "DEMO-PHONE", "phone_country": "0", "phone_provider_id": "2368,3243"},
	} {
		w := apiRequest(a.Handler(), "POST", "/api/redeem", body, "", nil, "")
		if w.Code != 409 {
			t.Fatalf("invalid choice %v: %d %s", body, w.Code, w.Body.String())
		}
	}
	if p.allocations != 0 {
		t.Fatal("invalid selection purchased")
	}
	body := map[string]string{"cdk": "DEMO-PHONE", "phone_country": "187", "phone_provider_id": "3243"}
	order := parseOrder(t, apiRequest(a.Handler(), "POST", "/api/redeem", body, "", nil, ""))
	if len(p.requests) != 1 || p.requests[0].Country != "187" || p.requests[0].ProviderID != "3243" || p.requests[0].MaxPrice != "0.12" {
		t.Fatalf("wrong purchase: %+v", p.requests)
	}
	if order.Order.PhoneChannel == nil || order.Order.PhoneChannel.ProviderID != "3243" {
		t.Fatal("selected channel not persisted/decorated")
	}
	// A replay without the selection resumes the active order, even when the
	// catalog is now unavailable. It must not initiate another purchase.
	p.catalogErr = errors.New("unavailable")
	replay := redeem(t, a, "DEMO-PHONE")
	if replay.Order.ID != order.Order.ID || p.allocations != 1 {
		t.Fatal("replay bought another phone")
	}
}

func TestAdminPhoneCatalogAuthAndAllCountries(t *testing.T) {
	a, p := catalogTestApp(t)
	for _, path := range []string{"/api/admin/phone/countries", "/api/admin/phone/channels"} {
		if w := apiRequest(a.Handler(), "GET", path, nil, "", nil, ""); w.Code != 401 {
			t.Fatalf("unauthorized catalog %d", w.Code)
		}
	}
	cookie := adminCookie(t, a)
	w := apiRequest(a.Handler(), "GET", "/api/admin/phone/channels?countries=*&max_price=0.20", nil, "", cookie, "")
	var v struct {
		Channels []provider.PhoneChannel `json:"channels"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if w.Code != http.StatusOK || len(v.Channels) != 3 {
		t.Fatalf("all-country preview %d %s", w.Code, w.Body.String())
	}
	a.settings.PhoneCountry = "*"
	body := map[string]string{"kind": "phone", "request_id": "catalog-admin-000001", "phone_country": "16", "phone_provider_id": "9000"}
	order := parseOrder(t, apiRequest(a.Handler(), "POST", "/api/admin/resources", body, "", cookie, ""))
	p.catalogErr = errors.New("offline")
	delete(body, "phone_country")
	delete(body, "phone_provider_id")
	replay := parseOrder(t, apiRequest(a.Handler(), "POST", "/api/admin/resources", body, "", cookie, ""))
	if replay.Order.ID != order.Order.ID || p.allocations != 1 {
		t.Fatal("administrator replay purchased another phone")
	}
}

func TestPhoneReplaceUsesNewSelectionAndRejectsStaleTokenReplay(t *testing.T) {
	a, p := catalogTestApp(t)
	o := parseOrder(t, apiRequest(a.Handler(), "POST", "/api/redeem", map[string]string{"cdk": "DEMO-PHONE", "phone_country": "0", "phone_provider_id": "2368"}, "", nil, ""))
	cancelled := parseOrder(t, apiRequest(a.Handler(), "POST", "/api/orders/cancel", nil, o.Token, nil, ""))
	if cancelled.Order.Status != "cancelled" {
		t.Fatal("cancel failed")
	}
	next := parseOrder(t, apiRequest(a.Handler(), "POST", "/api/orders/replace", map[string]string{"phone_country": "187", "phone_provider_id": "3243"}, o.Token, nil, ""))
	replay := parseOrder(t, apiRequest(a.Handler(), "POST", "/api/orders/replace", nil, o.Token, nil, ""))
	if next.Order.ID == o.Order.ID || replay.Order.ID != next.Order.ID || p.allocations != 2 || p.requests[1].ProviderID != "3243" {
		t.Fatal("replacement selection/idempotency failed")
	}
}
