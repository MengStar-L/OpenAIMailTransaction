package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"openai-mail-transaction/internal/provider"
	"strings"
	"sync"
	"testing"
	"time"
)

type inventoryTestProvider struct {
	*testProvider
	stockMu  sync.Mutex
	calls    int
	count    int
	err      error
	requests []provider.Request
}

func (p *inventoryTestProvider) EmailInventory(_ context.Context, r provider.Request) (provider.EmailInventory, error) {
	p.stockMu.Lock()
	defer p.stockMu.Unlock()
	p.calls++
	p.requests = append(p.requests, r)
	return provider.EmailInventory{Count: p.count}, p.err
}

func inventoryTestApp(t *testing.T, p *inventoryTestProvider) *App {
	t.Helper()
	opts := testOptions(t.TempDir(), p.testProvider)
	opts.Client = p
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func inventoryGet(t *testing.T, a *App) emailInventoryView {
	t.Helper()
	w := apiRequest(a.Handler(), http.MethodGet, "/api/inventory", nil, "", nil, "")
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "provider-secret") {
		t.Fatal("secret in inventory response")
	}
	var v struct {
		Email emailInventoryView `json:"email"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v.Email
}

func TestInventoryPublicCacheAndConfigurationChanges(t *testing.T) {
	p := &inventoryTestProvider{testProvider: &testProvider{}, count: 217}
	a := inventoryTestApp(t, p)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			a.inventory(w, httptest.NewRequest(http.MethodGet, "/api/inventory", nil))
			if w.Code != 200 {
				t.Errorf("status %d", w.Code)
			}
		}()
	}
	wg.Wait()
	v := inventoryGet(t, a)
	if v.Count == nil || *v.Count != 217 || v.Status != "available" || v.UpdatedAt == nil || p.calls != 1 {
		t.Fatalf("unexpected view %#v calls %d", v, p.calls)
	}
	a.settingsMu.Lock()
	a.settings.EmailDomain, a.settings.EmailMaxPrice = "icloud.com", "0.75"
	a.settingsMu.Unlock()
	inventoryGet(t, a)
	if p.calls != 2 || p.requests[1].Domain != "icloud.com" || p.requests[1].MaxPrice != "0.75" || p.requests[1].Service != "dr" {
		t.Fatal("configuration did not invalidate stock")
	}
	a.clientMu.Lock()
	a.providerKey = "provider-secret-new-key"
	a.clientMu.Unlock()
	inventoryGet(t, a)
	if p.calls != 3 {
		t.Fatal("credential change did not invalidate stock")
	}
	if n, _, _, _ := p.counts(); n != 0 {
		t.Fatal("inventory bought resources")
	}
}

func TestInventoryZeroUnknownAndFailedRefresh(t *testing.T) {
	p := &inventoryTestProvider{testProvider: &testProvider{}, count: 0}
	a := inventoryTestApp(t, p)
	v := inventoryGet(t, a)
	if v.Count == nil || *v.Count != 0 || v.Status != "available" {
		t.Fatalf("valid zero lost: %#v", v)
	}
	p.err = &provider.Error{Message: "provider-secret transport diagnostic"}
	a.inventoryCache.ExpiresAt = time.Time{}
	v = inventoryGet(t, a)
	if v.Count != nil || v.UpdatedAt != nil || v.Status != "unavailable" {
		t.Fatalf("failure misrepresented as stock: %#v", v)
	}
	inventoryGet(t, a)
	if p.calls != 2 {
		t.Fatal("failed lookup not cached")
	}
	p.err, p.count = nil, -1
	a.inventoryCache.ExpiresAt = time.Time{}
	if v = inventoryGet(t, a); v.Count != nil || v.Status != "unavailable" {
		t.Fatal("negative inventory accepted")
	}
}

func TestInventoryMissingConfigurationAndUnsupportedClient(t *testing.T) {
	opts := Options{DataDir: t.TempDir(), Mode: "live", DisableWorker: true}
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	v := inventoryGet(t, a)
	if v.Count != nil || v.Status != "unconfigured" {
		t.Fatal("missing key shown as stock")
	}
	p := &inventoryTestProvider{testProvider: &testProvider{}, err: errors.New("provider-secret")}
	a.clientMu.Lock()
	a.client = p
	a.clientMu.Unlock()
	v = inventoryGet(t, a)
	if v.Status != "unconfigured" || p.calls != 0 {
		t.Fatal("missing price made a provider request")
	}
	a.settingsMu.Lock()
	a.settings.EmailMaxPrice = "1"
	a.settingsMu.Unlock()
	a.clientMu.Lock()
	a.client = &testProvider{}
	a.clientMu.Unlock()
	v = inventoryGet(t, a)
	if v.Status != "unavailable" || v.Count != nil {
		t.Fatal("unsupported provider shown as stock")
	}
}

func TestInventoryAllocationRefusalRetainsReferenceStockAndSuccessClearsIt(t *testing.T) {
	p := &inventoryTestProvider{testProvider: &testProvider{allocationErrors: []error{
		&provider.Error{Code: "no_stock", Message: "No available mail"},
		&provider.Error{Code: "no_balance"},
	}}, count: 40}
	a := inventoryTestApp(t, p)
	before := inventoryGet(t, a)
	if before.AllocationStatus != "unconfirmed" || before.AllocationCheckedAt != nil || before.Message != "上游参考库存，实际以分配结果为准" {
		t.Fatalf("quote claimed confirmed availability: %+v", before)
	}
	failed := redeem(t, a, "DEMO-EMAIL")
	if failed.Order.Status != "queued" {
		t.Fatalf("expected definite refusal, got %s", failed.Order.Status)
	}
	refused := inventoryGet(t, a)
	if refused.Count == nil || *refused.Count != 40 || refused.AllocationStatus != "no_stock" || refused.AllocationCheckedAt == nil || refused.Message != "最近分配被上游拒绝，当前条件下暂无可分配邮箱" || p.calls != 2 {
		t.Fatalf("refusal did not invalidate quote cache while preserving reference count: %+v, calls %d", refused, p.calls)
	}
	// A fresh positive quote must not erase the more specific purchase result.
	a.inventoryMu.Lock()
	a.inventoryCache.ExpiresAt = time.Time{}
	a.inventoryMu.Unlock()
	refreshed := inventoryGet(t, a)
	if refreshed.Count == nil || *refreshed.Count != 40 || refreshed.AllocationStatus != "no_stock" || !refreshed.AllocationCheckedAt.Equal(*refused.AllocationCheckedAt) || p.calls != 3 {
		t.Fatalf("fresh quote erased allocation refusal: %+v, calls %d", refreshed, p.calls)
	}
	queueStep(t, a, failed.Order.ID)
	afterBalanceError := inventoryGet(t, a)
	if afterBalanceError.AllocationStatus != "no_stock" || afterBalanceError.AllocationCheckedAt == nil || !afterBalanceError.AllocationCheckedAt.Equal(*refused.AllocationCheckedAt) || p.calls != 4 {
		t.Fatalf("unrelated failure erased or renewed stock evidence: %+v, calls %d", afterBalanceError, p.calls)
	}
	p.stockMu.Lock()
	p.count = 39
	p.stockMu.Unlock()
	success := redeem(t, a, "DEMO-EMAIL")
	if success.Order.Status != "waiting" {
		t.Fatalf("allocation did not recover: %s", success.Order.Status)
	}
	view := inventoryGet(t, a)
	if view.Count == nil || *view.Count != 39 || view.AllocationStatus != "unconfirmed" || view.AllocationCheckedAt != nil || p.calls != 5 {
		t.Fatalf("successful purchase did not clear refusal and invalidate cache: %+v, calls %d", view, p.calls)
	}
}

func TestInventoryOtherAllocationErrorsDoNotClaimNoStock(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"balance", &provider.Error{Code: "no_balance"}},
		{"configuration", &provider.Error{Code: "upstream_configuration"}},
		{"transport", &provider.Error{Code: "upstream_unavailable", Uncertain: true}},
		{"uncertain_no_stock", &provider.Error{Code: "no_stock", Uncertain: true}},
		{"unknown", errors.New("unrecognized upstream failure")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &inventoryTestProvider{testProvider: &testProvider{allocationErrors: []error{tc.err}}, count: 40}
			a := inventoryTestApp(t, p)
			inventoryGet(t, a)
			redeem(t, a, "DEMO-EMAIL")
			view := inventoryGet(t, a)
			if view.Count == nil || *view.Count != 40 || view.AllocationStatus != "unconfirmed" || view.AllocationCheckedAt != nil || p.calls != 2 {
				t.Fatalf("non-stock error changed availability or failed to invalidate cache: %+v, calls %d", view, p.calls)
			}
		})
	}
}

func TestInventoryAllocationRefusalExpiresWithoutChangingCachedCount(t *testing.T) {
	p := &inventoryTestProvider{testProvider: &testProvider{allocationErrors: []error{&provider.Error{Code: "no_stock"}}}, count: 40}
	a := inventoryTestApp(t, p)
	redeem(t, a, "DEMO-EMAIL")
	view := inventoryGet(t, a)
	if view.AllocationStatus != "no_stock" {
		t.Fatalf("missing initial refusal: %+v", view)
	}
	a.inventoryMu.Lock()
	for key := range a.allocationObservations {
		a.allocationObservations[key] = allocationObservation{CheckedAt: time.Now().Add(-allocationObservationTTL - time.Second)}
	}
	a.inventoryMu.Unlock()
	view = inventoryGet(t, a)
	if view.Count == nil || *view.Count != 40 || view.AllocationStatus != "unconfirmed" || view.AllocationCheckedAt != nil || p.calls != 1 {
		t.Fatalf("expired observation contaminated cached quote: %+v, calls %d", view, p.calls)
	}
	if len(a.allocationObservations) != 0 {
		t.Fatal("expired observations were not pruned")
	}
}

func TestInventoryVoucherSnapshotRefusalDoesNotContaminateCurrentConfiguration(t *testing.T) {
	for _, field := range []string{"service", "domain", "price"} {
		t.Run(field, func(t *testing.T) {
			p := &inventoryTestProvider{testProvider: &testProvider{allocationErrors: []error{&provider.Error{Code: "no_stock"}}}, count: 40}
			a := inventoryTestApp(t, p)
			original := a.currentSettings()
			a.settingsMu.Lock()
			switch field {
			case "service":
				a.settings.EmailService = "different-service"
			case "domain":
				a.settings.EmailDomain = "icloud.com"
			case "price":
				a.settings.EmailMaxPrice = "0.20"
			}
			a.settingsMu.Unlock()
			inventoryGet(t, a)
			// The seeded voucher still contains the original service/domain/price.
			redeem(t, a, "DEMO-EMAIL")
			view := inventoryGet(t, a)
			if view.AllocationStatus != "unconfirmed" || view.AllocationCheckedAt != nil || p.calls != 1 {
				t.Fatalf("voucher snapshot refusal invalidated/contaminated unrelated settings: %+v, calls %d", view, p.calls)
			}
			a.settingsMu.Lock()
			a.settings = original
			a.settingsMu.Unlock()
			view = inventoryGet(t, a)
			if view.Count == nil || *view.Count != 40 || view.AllocationStatus != "no_stock" || view.AllocationCheckedAt == nil || p.calls != 2 {
				t.Fatalf("observation was not attached to the actual purchase parameters: %+v", view)
			}
		})
	}
}

func TestInventoryAllocationRefusalIsCredentialScoped(t *testing.T) {
	p := &inventoryTestProvider{testProvider: &testProvider{allocationErrors: []error{&provider.Error{Code: "no_stock"}}}, count: 40}
	a := inventoryTestApp(t, p)
	originalKey := a.currentAPIKey()
	redeem(t, a, "DEMO-EMAIL")
	if view := inventoryGet(t, a); view.AllocationStatus != "no_stock" {
		t.Fatalf("missing original credential observation: %+v", view)
	}
	a.clientMu.Lock()
	a.providerKey = "provider-secret-another-account"
	a.clientMu.Unlock()
	view := inventoryGet(t, a)
	if view.AllocationStatus != "unconfirmed" || view.AllocationCheckedAt != nil {
		t.Fatalf("previous account refusal leaked into new credential: %+v", view)
	}
	// A success under another credential must not clear the original refusal.
	s := a.currentSettings()
	a.providerGate.RLock()
	a.observeEmailAllocation(provider.Request{Kind: "email", Service: s.EmailService, Domain: s.EmailDomain, MaxPrice: s.EmailMaxPrice}, nil)
	a.providerGate.RUnlock()
	a.clientMu.Lock()
	a.providerKey = originalKey
	a.clientMu.Unlock()
	if view = inventoryGet(t, a); view.AllocationStatus != "no_stock" {
		t.Fatalf("unrelated account success cleared original refusal: %+v", view)
	}
}

func TestInventoryPhoneRefusalDoesNotChangeMailboxStock(t *testing.T) {
	p := &inventoryTestProvider{testProvider: &testProvider{allocationErrors: []error{&provider.Error{Code: "no_stock"}}}, count: 40}
	a := inventoryTestApp(t, p)
	inventoryGet(t, a)
	redeem(t, a, "DEMO-PHONE")
	view := inventoryGet(t, a)
	if view.AllocationStatus != "unconfirmed" || view.AllocationCheckedAt != nil || p.calls != 1 {
		t.Fatalf("phone refusal changed mailbox quote: %+v, calls %d", view, p.calls)
	}
}

func TestInventoryAllocationObservationsAreBounded(t *testing.T) {
	a := inventoryTestApp(t, &inventoryTestProvider{testProvider: &testProvider{}, count: 40})
	a.providerGate.RLock()
	defer a.providerGate.RUnlock()
	for i := range maxAllocationObservations + 1 {
		a.observeEmailAllocation(provider.Request{Kind: "email", Service: fmt.Sprintf("service-%d", i), Domain: "gmail.com", MaxPrice: "0.016"}, &provider.Error{Code: "no_stock"})
	}
	if len(a.allocationObservations) != maxAllocationObservations {
		t.Fatalf("unexpected retained observations: %d", len(a.allocationObservations))
	}
	latest := emailInventoryKey(provider.Request{Service: fmt.Sprintf("service-%d", maxAllocationObservations), Domain: "gmail.com", MaxPrice: "0.016"}, a.currentAPIKey())
	if _, exists := a.allocationObservations[latest]; !exists {
		t.Fatal("newest observation was discarded")
	}
}

type heldInventoryProvider struct {
	*inventoryTestProvider
	quoteEntered, releaseQuote, allocationReturned chan struct{}
	holdOnce                                       sync.Once
}

func (p *heldInventoryProvider) EmailInventory(ctx context.Context, req provider.Request) (provider.EmailInventory, error) {
	p.holdOnce.Do(func() {
		close(p.quoteEntered)
		select {
		case <-p.releaseQuote:
		case <-ctx.Done():
		}
	})
	return p.inventoryTestProvider.EmailInventory(ctx, req)
}

func (p *heldInventoryProvider) Allocate(ctx context.Context, req provider.Request) (provider.Activation, error) {
	activation, err := p.inventoryTestProvider.Allocate(ctx, req)
	close(p.allocationReturned)
	return activation, err
}

func TestInventoryInFlightQuoteCannotOverwriteAllocationRefusal(t *testing.T) {
	base := &inventoryTestProvider{testProvider: &testProvider{allocationErrors: []error{&provider.Error{Code: "no_stock"}}}, count: 40}
	p := &heldInventoryProvider{inventoryTestProvider: base, quoteEntered: make(chan struct{}), releaseQuote: make(chan struct{}), allocationReturned: make(chan struct{})}
	opts := testOptions(t.TempDir(), base.testProvider)
	opts.Client = p
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(p.releaseQuote) }) }
	defer release()
	queryDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		queryDone <- apiRequest(a.Handler(), http.MethodGet, "/api/inventory", nil, "", nil, "")
	}()
	select {
	case <-p.quoteEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("inventory request did not begin")
	}
	allocationDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		allocationDone <- apiRequest(a.Handler(), http.MethodPost, "/api/redeem", map[string]string{"cdk": "DEMO-EMAIL"}, "", nil, "")
	}()
	select {
	case <-p.allocationReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight inventory blocked the purchase itself")
	}
	release()
	select {
	case w := <-queryDone:
		if w.Code != http.StatusOK {
			t.Fatalf("query returned HTTP %d", w.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inventory query deadlocked after release")
	}
	select {
	case w := <-allocationDone:
		if order := parseOrder(t, w); order.Order.Status != "queued" {
			t.Fatalf("unexpected allocation result: %s", order.Order.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("allocation feedback deadlocked with stock query")
	}
	view := inventoryGet(t, a)
	if view.Count == nil || *view.Count != 40 || view.AllocationStatus != "no_stock" || view.AllocationCheckedAt == nil || base.calls != 2 {
		t.Fatalf("overlapping quote erased refusal or left stale cache: %+v, calls %d", view, base.calls)
	}
}
