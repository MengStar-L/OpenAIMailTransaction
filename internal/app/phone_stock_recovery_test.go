package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openai-mail-transaction/internal/provider"
)

func phoneStockError() error {
	return &provider.Error{Code: "no_stock", Message: "reference stock is unavailable"}
}

type countedPhoneCatalog struct {
	*catalogProvider
	reads atomic.Int32
}

func (p *countedPhoneCatalog) PhoneChannels(ctx context.Context, req provider.Request) ([]provider.PhoneChannel, error) {
	p.reads.Add(1)
	return p.catalogProvider.PhoneChannels(ctx, req)
}

func TestPhoneAllocationRetriesKeepExactSelection(t *testing.T) {
	p := &testProvider{allocationErrors: []error{phoneStockError(), phoneStockError(), nil}}
	req := provider.Request{Kind: "phone", Service: "dr", Country: "187", ProviderID: "3243", MaxPrice: "0.018", TTL: 20 * time.Minute}
	activation, err, exhausted := retryPhoneAllocation(context.Background(), p, req, time.Second, time.Millisecond)
	if err != nil || exhausted || activation.ID == "" || p.allocations != 3 {
		t.Fatalf("recovery: %v %v %+v, purchases=%d", err, exhausted, activation, p.allocations)
	}
	for _, purchased := range p.requests {
		if purchased != req {
			t.Fatalf("changed channel or price: %+v", purchased)
		}
	}
}

func TestPhoneAllocationRetryStopsOnAnyNonStockResult(t *testing.T) {
	for name, stop := range map[string]error{
		"unknown":         errors.New("unknown purchase outcome"),
		"uncertain_stock": &provider.Error{Code: "no_stock", Message: "uncertain", Uncertain: true},
		"balance":         &provider.Error{Code: "no_balance", Message: "insufficient balance"},
		"transport":       &provider.Error{Code: "transport", Message: "connection interrupted", Uncertain: true},
	} {
		t.Run(name, func(t *testing.T) {
			p := &testProvider{allocationErrors: []error{phoneStockError(), stop, nil}}
			_, err, exhausted := retryPhoneAllocation(context.Background(), p, provider.Request{Kind: "phone"}, time.Second, time.Millisecond)
			if err != stop || exhausted || p.allocations != 2 {
				t.Fatalf("unsafe retry after %s: %v exhausted=%v attempts=%d", name, err, exhausted, p.allocations)
			}
		})
	}
}

type phoneDeadlineProvider struct{ *testProvider }

func (p *phoneDeadlineProvider) Allocate(ctx context.Context, req provider.Request) (provider.Activation, error) {
	p.allocations++
	<-ctx.Done()
	return provider.Activation{}, &provider.Error{Code: "transport", Message: "deadline during purchase", Uncertain: true}
}

func TestPhoneAllocationDeadlineDuringPurchaseRemainsUncertain(t *testing.T) {
	p := &phoneDeadlineProvider{&testProvider{}}
	_, err, exhausted := retryPhoneAllocation(context.Background(), p, provider.Request{Kind: "phone"}, 15*time.Millisecond, time.Millisecond)
	var pe *provider.Error
	if !errors.As(err, &pe) || !pe.Uncertain || exhausted || p.allocations != 1 {
		t.Fatalf("deadline treated as no stock: %v %v %d", err, exhausted, p.allocations)
	}
}

func TestPhoneStockFailureWaitsThenHidesAndPreservesQuota(t *testing.T) {
	a, catalog := catalogTestApp(t)
	p := &countedPhoneCatalog{catalogProvider: catalog}
	a.client = p
	for range 20 {
		p.allocationErrors = append(p.allocationErrors, phoneStockError())
	}
	// Prime a multi-country cache; rejection of one exact channel invalidates
	// the snapshot and the next listing must perform a fresh read.
	_, err := a.readPhoneChannels(context.Background(), a.settings, false)
	if err != nil {
		t.Fatal(err)
	}
	choice := map[string]string{"cdk": "DEMO-PHONE", "phone_country": "0", "phone_provider_id": "2368"}
	started := time.Now()
	failed := parseOrder(t, apiRequest(a.Handler(), "POST", "/api/redeem", choice, "", nil, ""))
	if time.Since(started) < phoneAllocationWindow || failed.Order.Status != "failed" || failed.Order.Message != phoneUnavailableMessage || !failed.Order.CanRetry {
		t.Fatalf("failed before window or wrong failure: elapsed=%v order=%+v", time.Since(started), failed.Order)
	}
	if p.allocations < 2 || p.allocations > 11 || len(a.phoneCatalogCache) != 0 {
		t.Fatalf("wrong attempts/cache: %d %d", p.allocations, len(a.phoneCatalogCache))
	}
	assertCDK(t, a, failed.Order, "available", 0)
	var used int
	if err := a.db.QueryRow("SELECT used_count FROM cdks WHERE hash=?", hash("DEMO-PHONE")).Scan(&used); err != nil || used != 0 {
		t.Fatalf("failed allocation consumed quota: %d %v", used, err)
	}
	reads := p.reads.Load()
	channels, err := a.readPhoneChannels(context.Background(), a.settings, false)
	if err != nil || len(channels) != 1 || channels[0].ProviderID != "3243" || p.reads.Load() != reads+1 {
		t.Fatalf("stale/unfiltered catalog: %+v %v reads=%d", channels, err, p.reads.Load())
	}
	allocations, reads := p.allocations, p.reads.Load()
	w := apiRequest(a.Handler(), "POST", "/api/redeem", choice, "", nil, "")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "10 分钟") || p.allocations != allocations || p.reads.Load() != reads {
		t.Fatalf("hidden selection bought again: %d %s", w.Code, w.Body.String())
	}
	key := [4]string{hash(a.currentAPIKey()), a.settings.PhoneService, "0", "2368"}
	if remaining := time.Until(a.phoneUnavailable[key]); remaining < 9*time.Minute || remaining > 10*time.Minute {
		t.Fatalf("hidden for %v", remaining)
	}
	// Expiry restores the channel even from a still-valid reference cache.
	a.phoneUnavailable[key] = time.Now().Add(-time.Second)
	channels, err = a.readPhoneChannels(context.Background(), a.settings, false)
	if err != nil || len(channels) != 2 {
		t.Fatalf("channel did not return after expiry: %+v %v", channels, err)
	}
	p.allocationErrors = nil
	ready := parseOrder(t, apiRequest(a.Handler(), "POST", "/api/redeem", choice, "", nil, ""))
	if ready.Order.Status != "waiting" || p.allocations != allocations+1 {
		t.Fatal("expired hidden channel could not be selected")
	}
}

func TestPhoneStockHiddenScopeAndPersistence(t *testing.T) {
	options := testOptions(t.TempDir(), &testProvider{})
	a, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	req := provider.Request{Kind: "phone", Service: "dr", Country: "0", ProviderID: "3243", MaxPrice: "0.018"}
	if err := a.markPhoneUnavailable(req); err != nil {
		_ = a.Close()
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = New(options)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	choice := phoneSelection{Country: "0", ProviderID: "3243"}
	if !a.isPhoneUnavailable("dr", choice) || a.isPhoneUnavailable("dr", phoneSelection{Country: "187", ProviderID: "3243"}) || a.isPhoneUnavailable("dr", phoneSelection{Country: "0", ProviderID: "other"}) || a.isPhoneUnavailable("another", choice) {
		t.Fatal("hidden state did not persist or leaked across channel scope")
	}
	a.providerKey = "different-credential"
	if a.isPhoneUnavailable("dr", choice) {
		t.Fatal("hidden state leaked across credentials")
	}
	a.providerKey = ""
	// A price edit must not reset the same channel's wait.
	a.settings.PhoneCountry, a.settings.PhoneMaxPrice = "0", "99"
	a.client = &catalogProvider{testProvider: options.Client.(*testProvider)}
	if _, _, err := a.selectedPhoneRequest(a.settings, choice); err == nil || err.Error() != phoneUnavailableMessage {
		t.Fatal("price change bypassed hidden state")
	}
}

func TestUncertainPhonePurchaseNeverRetriesOrHides(t *testing.T) {
	for _, failure := range []error{errors.New("unknown result"), &provider.Error{Code: "no_stock", Message: "unconfirmed", Uncertain: true}} {
		t.Run(fmt.Sprintf("%T", failure), func(t *testing.T) {
			a, p := catalogTestApp(t)
			p.allocationErrors = []error{failure}
			body := map[string]string{"cdk": "DEMO-PHONE", "phone_country": "0", "phone_provider_id": "2368"}
			first := parseOrder(t, apiRequest(a.Handler(), "POST", "/api/redeem", body, "", nil, ""))
			replay := redeem(t, a, "DEMO-PHONE")
			if first.Order.Status != "review" || first.Order.CanRetry || replay.Order.ID != first.Order.ID || p.allocations != 1 || len(a.phoneUnavailable) != 0 {
				t.Fatalf("uncertain allocation was retried/hidden: %+v", first.Order)
			}
			assertCDK(t, a, first.Order, "review", 1)
		})
	}
}

type delayedStockCatalog struct {
	*catalogProvider
	started chan struct{}
	release chan struct{}
}

func (p *delayedStockCatalog) PhoneChannels(ctx context.Context, req provider.Request) ([]provider.PhoneChannel, error) {
	close(p.started)
	select {
	case <-p.release:
		return p.catalogProvider.PhoneChannels(ctx, req)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestPhoneStockRejectionFiltersCatalogAlreadyInFlight(t *testing.T) {
	a, base := catalogTestApp(t)
	p := &delayedStockCatalog{catalogProvider: base, started: make(chan struct{}), release: make(chan struct{})}
	a.client = p
	result := make(chan []provider.PhoneChannel, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		rows, _ := a.readPhoneChannels(ctx, a.settings, true)
		result <- rows
	}()
	<-p.started
	hidden := make(chan error, 1)
	go func() {
		hidden <- a.markPhoneUnavailable(provider.Request{Kind: "phone", Service: a.settings.PhoneService, Country: "0", ProviderID: "2368"})
	}()
	select {
	case err := <-hidden:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("stock rejection was blocked by slow catalog request")
	}
	close(p.release)
	rows := <-result
	if len(rows) != 1 || rows[0].ProviderID != "3243" {
		b, _ := json.Marshal(rows)
		t.Fatalf("in-flight result reintroduced unavailable channel: %s", b)
	}
}
