package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"openai-mail-transaction/internal/provider"
)

type balanceTestProvider struct {
	*testProvider
	balanceMu sync.Mutex
	calls     int
	amount    string
	err       error
	lookup    func(context.Context) (provider.Balance, error)
}

func (p *balanceTestProvider) Balance(ctx context.Context) (provider.Balance, error) {
	p.balanceMu.Lock()
	p.calls++
	amount, err, lookup := p.amount, p.err, p.lookup
	p.balanceMu.Unlock()
	if lookup != nil {
		return lookup(ctx)
	}
	return provider.Balance{Amount: amount}, err
}

func (p *balanceTestProvider) callCount() int {
	p.balanceMu.Lock()
	defer p.balanceMu.Unlock()
	return p.calls
}

func balanceTestApp(t *testing.T, p *balanceTestProvider) (*App, *http.Cookie) {
	t.Helper()
	opts := testOptions(t.TempDir(), p.testProvider)
	opts.Mode, opts.APIKey, opts.Client = "live", "balance-provider-secret", p
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, setupTestAdmin(t, a, testAdminPassword)
}

func balanceGet(t *testing.T, a *App, cookie *http.Cookie, force bool) balanceView {
	t.Helper()
	path := "/api/admin/balance"
	if force {
		path += "?refresh=1"
	}
	w := apiRequest(a.Handler(), http.MethodGet, path, nil, "", cookie, "")
	if w.Code != http.StatusOK {
		t.Fatalf("balance HTTP %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "provider-secret") || strings.Contains(w.Body.String(), "api_key") {
		t.Fatal("balance response leaked credentials")
	}
	var view balanceView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func ageBalanceCache(a *App, expired bool) {
	a.balanceMu.Lock()
	a.balanceCache.FetchedAt = time.Now().Add(-3 * time.Second)
	if expired {
		a.balanceCache.ExpiresAt = time.Time{}
	}
	a.balanceMu.Unlock()
}

func TestBalanceAdministratorOnlyAndNoPublicLeak(t *testing.T) {
	p := &balanceTestProvider{testProvider: &testProvider{}, amount: "2.1419"}
	a, cookie := balanceTestApp(t, p)
	expired := &http.Cookie{Name: "atelier_admin", Value: "expired-balance-session"}
	a.sessions.Store(hash(expired.Value), time.Now().Add(-time.Minute))
	for _, credentials := range []struct {
		token  string
		cookie *http.Cookie
	}{{}, {token: a.orderToken("some-order")}, {cookie: &http.Cookie{Name: "atelier_admin", Value: "forged"}}, {cookie: expired}} {
		w := apiRequest(a.Handler(), http.MethodGet, "/api/admin/balance?refresh=1", nil, credentials.token, credentials.cookie, "")
		if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "2.1419") {
			t.Fatalf("unauthorized balance response: HTTP %d %s", w.Code, w.Body.String())
		}
	}
	if p.callCount() != 0 {
		t.Fatal("unauthorized request queried upstream balance")
	}
	view := balanceGet(t, a, cookie, false)
	if view.Balance == nil || *view.Balance != "2.1419" || !view.Available || view.Status != "available" || view.Currency != "USD" || view.Mode != "live" || view.UpdatedAt == nil {
		t.Fatalf("admin balance = %+v", view)
	}
	w := apiRequest(a.Handler(), http.MethodGet, "/api/config", nil, "", nil, "")
	if strings.Contains(w.Body.String(), "2.1419") || strings.Contains(w.Body.String(), `"balance"`) {
		t.Fatal("public configuration exposed balance")
	}
	if n, _, _, _ := p.counts(); n != 0 {
		t.Fatal("balance lookup purchased resources")
	}
}

func TestBalanceCachesAndCoalescesConcurrentForcedRefreshes(t *testing.T) {
	p := &balanceTestProvider{testProvider: &testProvider{}, amount: "12.0010"}
	a, cookie := balanceTestApp(t, p)
	balanceGet(t, a, cookie, false)
	balanceGet(t, a, cookie, false)
	balanceGet(t, a, cookie, true)
	if p.callCount() != 1 {
		t.Fatal("immediate refresh bypassed minimum request interval")
	}
	ageBalanceCache(a, false)
	balanceGet(t, a, cookie, false)
	if p.callCount() != 1 {
		t.Fatal("ordinary request bypassed fifteen-second cache")
	}
	entered, release := make(chan struct{}, 1), make(chan struct{})
	p.balanceMu.Lock()
	p.lookup = func(ctx context.Context) (provider.Balance, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return provider.Balance{Amount: "11.9010"}, nil
		case <-ctx.Done():
			return provider.Balance{}, ctx.Err()
		}
	}
	p.balanceMu.Unlock()
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := apiRequest(a.Handler(), http.MethodGet, "/api/admin/balance?refresh=1", nil, "", cookie, "")
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "11.9010") {
				t.Errorf("coalesced balance HTTP %d: %s", w.Code, w.Body.String())
			}
		}()
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Error("forced lookup did not start")
	}
	close(release)
	wg.Wait()
	if p.callCount() != 2 {
		t.Fatalf("forced lookups were not coalesced: %d", p.callCount())
	}
}

func TestBalanceFailuresAreUnknownAndCachedSeparatelyFromZero(t *testing.T) {
	p := &balanceTestProvider{testProvider: &testProvider{}, amount: "0.0000"}
	a, cookie := balanceTestApp(t, p)
	view := balanceGet(t, a, cookie, false)
	if view.Balance == nil || *view.Balance != "0.0000" || !view.Available {
		t.Fatal("real zero was lost")
	}
	p.balanceMu.Lock()
	p.err = errors.New("balance-provider-secret https://private.example/?api_key=secret")
	p.balanceMu.Unlock()
	ageBalanceCache(a, true)
	view = balanceGet(t, a, cookie, false)
	if view.Balance != nil || view.Available || view.UpdatedAt != nil || view.Status != "unavailable" {
		t.Fatalf("failure was shown as zero/stale balance: %+v", view)
	}
	ageBalanceCache(a, false)
	balanceGet(t, a, cookie, true)
	if p.callCount() != 2 {
		t.Fatal("forced refresh bypassed five-second error cache")
	}
	p.balanceMu.Lock()
	p.err, p.amount = nil, "bad secret balance-provider-secret"
	p.balanceMu.Unlock()
	ageBalanceCache(a, true)
	view = balanceGet(t, a, cookie, false)
	if view.Balance != nil || view.Available {
		t.Fatal("malformed optional provider result reached UI")
	}
}

func TestBalanceCredentialChangeDoesNotReusePreviousAccount(t *testing.T) {
	first := &balanceTestProvider{testProvider: &testProvider{}, amount: "17.0000"}
	a, cookie := balanceTestApp(t, first)
	balanceGet(t, a, cookie, false)
	second := &balanceTestProvider{testProvider: &testProvider{}, amount: "23.0000"}
	a.providerGate.Lock()
	a.clientMu.Lock()
	a.providerKey, a.client = "balance-provider-secret-second", second
	a.clientMu.Unlock()
	a.providerGate.Unlock()
	view := balanceGet(t, a, cookie, false)
	if view.Balance == nil || *view.Balance != "23.0000" || first.callCount() != 1 || second.callCount() != 1 {
		t.Fatalf("account switch reused balance cache: %+v", view)
	}
}

func TestBalanceCredentialRotationWaitsForInFlightAccountRead(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	first := &balanceTestProvider{testProvider: &testProvider{}, lookup: func(ctx context.Context) (provider.Balance, error) {
		close(entered)
		select {
		case <-release:
			return provider.Balance{Amount: "17.0000"}, nil
		case <-ctx.Done():
			return provider.Balance{}, ctx.Err()
		}
	}}
	a, cookie := balanceTestApp(t, first)
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		finished <- apiRequest(a.Handler(), http.MethodGet, "/api/admin/balance", nil, "", cookie, "")
	}()
	<-entered
	second := &balanceTestProvider{testProvider: &testProvider{}, amount: "23.0000"}
	attempted, changed := make(chan struct{}), make(chan struct{})
	go func() {
		close(attempted)
		a.providerGate.Lock()
		a.clientMu.Lock()
		a.providerKey, a.client = "balance-provider-secret-second", second
		a.clientMu.Unlock()
		a.providerGate.Unlock()
		close(changed)
	}()
	<-attempted
	select {
	case <-changed:
		t.Error("account changed before its in-flight read completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	w := <-finished
	<-changed
	if !strings.Contains(w.Body.String(), `"balance":"17.0000"`) {
		t.Fatalf("first read switched accounts: %s", w.Body.String())
	}
	view := balanceGet(t, a, cookie, false)
	if view.Balance == nil || *view.Balance != "23.0000" || second.callCount() != 1 {
		t.Fatalf("new account reused old in-flight result: %+v", view)
	}
}

func TestBalanceMissingConfigurationUnsupportedAndDemo(t *testing.T) {
	for _, mode := range []string{"live", "demo"} {
		t.Run(mode, func(t *testing.T) {
			a, err := New(Options{DataDir: t.TempDir(), Mode: mode, DisableWorker: true})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			cookie := setupTestAdmin(t, a, testAdminPassword)
			view := balanceGet(t, a, cookie, false)
			if mode == "demo" {
				if view.Status != "demo" || !strings.Contains(view.Message, "模拟") || view.Balance == nil || *view.Balance != "100.00" {
					t.Fatalf("demo balance not labeled: %+v", view)
				}
			} else if view.Status != "unconfigured" || view.Available || view.Balance != nil {
				t.Fatalf("missing key shown as balance: %+v", view)
			}
			a.clientMu.Lock()
			a.client = &testProvider{}
			a.clientMu.Unlock()
			view = balanceGet(t, a, cookie, false)
			if view.Status != "unavailable" || view.Balance != nil {
				t.Fatal("unsupported client shown as balance")
			}
		})
	}
}

func TestBalanceLookupHasDeadlineAndAppShutdownCancelsIt(t *testing.T) {
	entered := make(chan context.Context, 1)
	p := &balanceTestProvider{testProvider: &testProvider{}, lookup: func(ctx context.Context) (provider.Balance, error) {
		entered <- ctx
		<-ctx.Done()
		return provider.Balance{}, ctx.Err()
	}}
	a, _ := balanceTestApp(t, p)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		a.adminBalance(w, httptest.NewRequest(http.MethodGet, "/api/admin/balance", nil))
		done <- w
	}()
	ctx := <-entered
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > balanceTimeout {
		t.Fatal("balance lookup has no bounded deadline")
	}
	a.cancel()
	select {
	case w := <-done:
		if !strings.Contains(w.Body.String(), `"balance":null`) {
			t.Fatal("cancelled request returned a known balance")
		}
	case <-time.After(time.Second):
		t.Fatal("balance lookup did not stop on app shutdown")
	}
}
