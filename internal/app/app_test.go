package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"openai-mail-transaction/internal/provider"
)

const testAdminPassword = "integration-password-2026"

// The fake preserves the real provider's failure distinction: a rejected
// allocation has no resource, whereas an uncertain allocation might be charged.
type testProvider struct {
	mu                                             sync.Mutex
	allocations, polls, cancellations, completions int
	requests                                       []provider.Request
	allocationErrors                               []error
	pollResult                                     provider.Result
	pollError, cancelError, completeError          error
	cancelDelay                                    time.Duration
}

func (p *testProvider) Allocate(_ context.Context, req provider.Request) (provider.Activation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.allocations++
	p.requests = append(p.requests, req)
	if len(p.allocationErrors) > 0 {
		err := p.allocationErrors[0]
		p.allocationErrors = p.allocationErrors[1:]
		if err != nil {
			return provider.Activation{}, err
		}
	}
	return provider.Activation{ID: fmt.Sprintf("activation-%d", p.allocations), Resource: "+12025550123", ExpiresAt: time.Now().Add(15 * time.Minute), CancelAfter: time.Now().Add(p.cancelDelay)}, nil
}

func (p *testProvider) Poll(context.Context, string, string) (provider.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.polls++
	if p.pollResult.Status == "" {
		return provider.Result{Status: "waiting"}, p.pollError
	}
	return p.pollResult, p.pollError
}

func (p *testProvider) Cancel(context.Context, string, string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancellations++
	return p.cancelError
}

func (p *testProvider) Complete(context.Context, string, string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.completions++
	return p.completeError
}

func (p *testProvider) counts() (int, int, int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.allocations, p.polls, p.cancellations, p.completions
}

func newTestApp(t *testing.T, p *testProvider) *App {
	t.Helper()
	a, err := New(testOptions(t.TempDir(), p))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	setupTestAdmin(t, a, testAdminPassword)
	return a
}

func testOptions(dir string, p *testProvider) Options {
	return Options{DataDir: dir, Mode: "demo", Client: p, DisableWorker: true, Logger: log.New(io.Discard, "", 0)}
}

func apiRequest(handler http.Handler, method, path string, body any, token string, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, "http://example.test"+path, bytes.NewReader(payload))
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

type orderEnvelope struct {
	Token string `json:"token"`
	Order Order  `json:"order"`
}

func parseOrder(t *testing.T, w *httptest.ResponseRecorder) orderEnvelope {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}
	var v orderEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Order.ID == "" {
		t.Fatalf("missing order: %s", w.Body.String())
	}
	return v
}

func redeem(t *testing.T, a *App, code string) orderEnvelope {
	t.Helper()
	return parseOrder(t, apiRequest(a.Handler(), http.MethodPost, "/api/redeem", map[string]string{"cdk": code}, "", nil, ""))
}

func assertCDK(t *testing.T, a *App, order Order, status string, attempts int) {
	t.Helper()
	stored, err := a.getOrder(order.ID)
	if err != nil {
		t.Fatal(err)
	}
	c, err := a.getCDK(stored.CDKID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != status || c.Attempts != attempts {
		t.Fatalf("CDK = (%s, %d), want (%s, %d)", c.Status, c.Attempts, status, attempts)
	}
}

func TestConcurrentRedemptionAllocatesOnlyOnce(t *testing.T) {
	p := &testProvider{}
	a := newTestApp(t, p)
	h := a.Handler()
	const clients = 8
	responses := make(chan *httptest.ResponseRecorder, clients)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			responses <- apiRequest(h, http.MethodPost, "/api/redeem", map[string]string{"cdk": " demo-phone "}, "", nil, "")
		}()
	}
	close(start)
	wg.Wait()
	close(responses)
	var id, token string
	for response := range responses {
		v := parseOrder(t, response)
		if id == "" {
			id, token = v.Order.ID, v.Token
		}
		if v.Order.ID != id || v.Token != token {
			t.Fatal("concurrent redemption created different orders or sessions")
		}
	}
	if count, _, _, _ := p.counts(); count != 1 {
		t.Fatalf("allocation count = %d, want 1", count)
	}
}

func TestRejectedAllocationPreservesAttempt(t *testing.T) {
	p := &testProvider{allocationErrors: []error{&provider.Error{Code: "no_numbers", Message: "暂无资源"}}}
	a := newTestApp(t, p)
	first := redeem(t, a, "DEMO-PHONE")
	if first.Order.Status != "failed" || !first.Order.CanRetry {
		t.Fatalf("rejected allocation: %+v", first.Order)
	}
	assertCDK(t, a, first.Order, "available", 0)
	second := redeem(t, a, "DEMO-PHONE")
	if second.Order.ID == first.Order.ID || second.Order.Status != "waiting" || second.Order.Attempt != 1 {
		t.Fatalf("retry did not start first funded attempt: %+v", second.Order)
	}
	assertCDK(t, a, second.Order, "active", 1)
}

func TestUncertainAllocationRequiresReviewWithoutAutomaticRetry(t *testing.T) {
	p := &testProvider{allocationErrors: []error{&provider.Error{Code: "timeout", Message: "结果待核对", Uncertain: true}}}
	a := newTestApp(t, p)
	first := redeem(t, a, "DEMO-EMAIL")
	second := redeem(t, a, "DEMO-EMAIL")
	if first.Order.Status != "review" || first.Order.CanRetry || second.Order.ID != first.Order.ID {
		t.Fatalf("uncertain purchase was not retained: first=%+v second=%+v", first.Order, second.Order)
	}
	assertCDK(t, a, first.Order, "review", 1)
	if count, _, _, _ := p.counts(); count != 1 {
		t.Fatalf("uncertain purchase allocated %d times", count)
	}
}

func TestCancellationFailureKeepsVoucherReserved(t *testing.T) {
	p := &testProvider{cancelError: &provider.Error{Code: "transport", Message: "取消结果待确认"}}
	a := newTestApp(t, p)
	first := redeem(t, a, "DEMO-PHONE")
	cancelled := parseOrder(t, apiRequest(a.Handler(), http.MethodPost, "/api/orders/cancel", map[string]any{}, first.Token, nil, ""))
	if cancelled.Order.Status != "cancel_pending" || cancelled.Order.CanRetry {
		t.Fatalf("failed cancellation released voucher: %+v", cancelled.Order)
	}
	assertCDK(t, a, cancelled.Order, "active", 1)
	resumed := redeem(t, a, "DEMO-PHONE")
	if resumed.Order.ID != first.Order.ID {
		t.Fatal("cancellation failure permitted a new order")
	}
	if count, _, _, _ := p.counts(); count != 1 {
		t.Fatalf("allocation count = %d", count)
	}
}

func TestConfirmedCancellationDoesNotConsumeSuccessfulUseQuota(t *testing.T) {
	p := &testProvider{}
	a := newTestApp(t, p)
	first := redeem(t, a, "DEMO-PHONE")
	stored, err := a.getOrder(first.Order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec("UPDATE cdks SET max_attempts=2 WHERE id=?", stored.CDKID); err != nil {
		t.Fatal(err)
	}
	cancelled := parseOrder(t, apiRequest(a.Handler(), http.MethodPost, "/api/orders/cancel", map[string]any{}, first.Token, nil, ""))
	if cancelled.Order.Status != "cancelled" || !cancelled.Order.CanRetry {
		t.Fatalf("confirmed release did not allow retry: %+v", cancelled.Order)
	}
	second := redeem(t, a, "DEMO-PHONE")
	if second.Order.ID == first.Order.ID || second.Order.Attempt != 2 {
		t.Fatalf("second attempt: %+v", second.Order)
	}
	terminal := parseOrder(t, apiRequest(a.Handler(), http.MethodPost, "/api/orders/cancel", map[string]any{}, second.Token, nil, ""))
	if !terminal.Order.CanRetry || terminal.Order.UsedCount != 0 {
		t.Fatal("confirmed cancellation consumed successful-use quota")
	}
	third := redeem(t, a, "DEMO-PHONE")
	if third.Order.ID == second.Order.ID {
		t.Fatal("old attempt limit prevented another uncharged resource")
	}
	assertCDK(t, a, third.Order, "active", 3)
	if count, _, _, _ := p.counts(); count != 3 {
		t.Fatalf("allocation count = %d, want 3", count)
	}
}

func TestCodeDeliverySpendsVoucherAndCannotBeCancelled(t *testing.T) {
	p := &testProvider{pollResult: provider.Result{Status: "received", Code: "083741"}}
	a := newTestApp(t, p)
	first := redeem(t, a, "DEMO-PHONE")
	received := parseOrder(t, apiRequest(a.Handler(), http.MethodGet, "/api/orders/current", nil, first.Token, nil, ""))
	if received.Order.Status != "received" || received.Order.Code != "083741" || received.Order.CanRetry {
		t.Fatalf("received code: %+v", received.Order)
	}
	assertCDK(t, a, received.Order, "active", 1)
	apiRequest(a.Handler(), http.MethodPost, "/api/orders/cancel", map[string]any{}, first.Token, nil, "")
	assertCDK(t, a, received.Order, "active", 1)
	if _, _, cancels, _ := p.counts(); cancels != 0 {
		t.Fatal("received activation was sent to upstream cancellation")
	}
	completed := parseOrder(t, apiRequest(a.Handler(), http.MethodPost, "/api/orders/complete", map[string]any{}, first.Token, nil, ""))
	if completed.Order.Status != "completed" || completed.Order.CanRetry {
		t.Fatalf("completed order: %+v", completed.Order)
	}
	resumed := redeem(t, a, "DEMO-PHONE")
	if resumed.Order.ID != first.Order.ID || resumed.Order.Code != "083741" {
		t.Fatal("spent voucher did not resume its existing result")
	}
	if count, _, _, completes := p.counts(); count != 1 || completes != 1 {
		t.Fatalf("allocations/completions = %d/%d", count, completes)
	}
}

func TestCancelDelayAcceptsIntentBeforeUpstreamCall(t *testing.T) {
	p := &testProvider{cancelDelay: time.Minute}
	a := newTestApp(t, p)
	first := redeem(t, a, "DEMO-PHONE")
	for range 2 {
		cancelled := parseOrder(t, apiRequest(a.Handler(), http.MethodPost, "/api/orders/cancel", map[string]any{}, first.Token, nil, ""))
		if cancelled.Order.Status != "cancel_pending" || cancelled.Order.CanRetry || cancelled.Order.UsedCount != 0 {
			t.Fatalf("cancel intent did not retain the uncharged order: %+v", cancelled.Order)
		}
	}
	stored, err := a.getOrder(first.Order.ID)
	if err != nil || stored.Status != "cancel_pending" || stored.CancelReason != "cancelled" {
		t.Fatalf("cancel intent was not persisted: %+v, err %v", stored, err)
	}
	if _, polls, cancels, _ := p.counts(); polls != 0 || cancels != 0 {
		t.Fatalf("queued cancel waited on the provider: polls=%d, cancellations=%d", polls, cancels)
	}
	assertCDK(t, a, first.Order, "active", 1)
}

func TestAdminAuthenticationAndSameOriginProtection(t *testing.T) {
	a := newTestApp(t, &testProvider{})
	h := a.Handler()
	for _, path := range []string{"/api/admin/session", "/api/admin/overview", "/api/admin/cdks", "/api/admin/settings"} {
		if w := apiRequest(h, http.MethodGet, path, nil, "", nil, ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s: HTTP %d", path, w.Code)
		}
	}
	foreign := apiRequest(h, http.MethodPost, "/api/admin/login", map[string]string{"password": testAdminPassword}, "", nil, "https://attacker.test")
	if foreign.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login HTTP %d", foreign.Code)
	}
	wrong := apiRequest(h, http.MethodPost, "/api/admin/login", map[string]string{"password": "incorrect-password"}, "", nil, "http://example.test")
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password HTTP %d", wrong.Code)
	}
	login := apiRequest(h, http.MethodPost, "/api/admin/login", map[string]string{"password": testAdminPassword}, "", nil, "http://example.test")
	if login.Code != http.StatusOK {
		t.Fatalf("login HTTP %d: %s", login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not set a session cookie")
	}
	cookie := cookies[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("unsafe session cookie: %+v", cookie)
	}
	if w := apiRequest(h, http.MethodGet, "/api/admin/session", nil, "", cookie, ""); w.Code != http.StatusOK {
		t.Fatalf("session HTTP %d", w.Code)
	}
	if w := apiRequest(h, http.MethodPut, "/api/admin/settings", defaultSettings(), "", cookie, "https://attacker.test"); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin settings HTTP %d", w.Code)
	}
	logout := apiRequest(h, http.MethodPost, "/api/admin/logout", map[string]any{}, "", cookie, "http://example.test")
	if logout.Code != http.StatusOK {
		t.Fatalf("logout HTTP %d", logout.Code)
	}
	if w := apiRequest(h, http.MethodGet, "/api/admin/session", nil, "", cookie, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("logged-out session still valid: HTTP %d", w.Code)
	}
}

func TestExpiredVoucherCannotAllocate(t *testing.T) {
	p := &testProvider{}
	a := newTestApp(t, p)
	if _, err := a.db.Exec("UPDATE cdks SET expires_at=? WHERE hash=?", time.Now().Add(-time.Minute).UnixMilli(), hash("DEMO-PHONE")); err != nil {
		t.Fatal(err)
	}
	w := apiRequest(a.Handler(), http.MethodPost, "/api/redeem", map[string]string{"cdk": "DEMO-PHONE"}, "", nil, "")
	if w.Code < 400 || w.Code >= 500 {
		t.Fatalf("expired voucher HTTP %d: %s", w.Code, w.Body.String())
	}
	if count, _, _, _ := p.counts(); count != 0 {
		t.Fatal("expired voucher reached upstream allocation")
	}
}

func TestExpiredActivationRequiresConfirmedRelease(t *testing.T) {
	for _, cancelFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel_failure_%t", cancelFails), func(t *testing.T) {
			p := &testProvider{}
			if cancelFails {
				p.cancelError = &provider.Error{Code: "network", Message: "释放结果待确认"}
			}
			a := newTestApp(t, p)
			first := redeem(t, a, "DEMO-EMAIL")
			if _, err := a.db.Exec("UPDATE orders SET expires_at=?,last_poll=0 WHERE id=?", time.Now().Add(-time.Minute).UnixMilli(), first.Order.ID); err != nil {
				t.Fatal(err)
			}
			current := parseOrder(t, apiRequest(a.Handler(), http.MethodGet, "/api/orders/current", nil, first.Token, nil, ""))
			if cancelFails {
				if current.Order.Status != "cancel_pending" || current.Order.CanRetry {
					t.Fatalf("local expiry incorrectly released unresolved resource: %+v", current.Order)
				}
				assertCDK(t, a, current.Order, "active", 1)
			} else {
				if current.Order.Status != "expired" || !current.Order.CanRetry {
					t.Fatalf("confirmed expiry failed to release resource: %+v", current.Order)
				}
				assertCDK(t, a, current.Order, "available", 1)
			}
			if _, _, cancels, _ := p.counts(); cancels != 1 {
				t.Fatalf("expiry cancellation attempts = %d, want 1", cancels)
			}
		})
	}
}

func TestLateCodeWinsOverUserCancellation(t *testing.T) {
	p := &testProvider{pollResult: provider.Result{Status: "received", Code: "902103"}}
	a := newTestApp(t, p)
	first := redeem(t, a, "DEMO-PHONE")
	result := parseOrder(t, apiRequest(a.Handler(), http.MethodPost, "/api/orders/cancel", map[string]any{}, first.Token, nil, ""))
	if result.Order.Status != "received" || result.Order.Code != "902103" || result.Order.CanRetry {
		t.Fatalf("code received during cancellation was lost: %+v", result.Order)
	}
	assertCDK(t, a, result.Order, "active", 1)
	if _, _, cancels, _ := p.counts(); cancels != 0 {
		t.Fatal("upstream cancellation attempted after late code was observed")
	}
}

func TestRestartRecoversUnfinishedPurchaseWithoutDuplicateAllocation(t *testing.T) {
	dir := t.TempDir()
	p := &testProvider{}
	a, err := New(testOptions(dir, p))
	if err != nil {
		t.Fatal(err)
	}
	first := redeem(t, a, "DEMO-PHONE")
	// Simulate a process exit after starting the purchase but before the
	// provider result was committed. Its charge cannot be inferred on restart.
	if _, err = a.db.Exec("UPDATE orders SET status='allocating',provider_id='',resource='' WHERE id=?", first.Order.ID); err != nil {
		a.Close()
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(testOptions(dir, p))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	resumed := redeem(t, reopened, "DEMO-PHONE")
	if resumed.Order.Status != "review" || resumed.Order.CanRetry || resumed.Order.ID != first.Order.ID {
		t.Fatalf("restart recovery: %+v", resumed.Order)
	}
	assertCDK(t, reopened, resumed.Order, "review", 1)
	if count, _, _, _ := p.counts(); count != 1 {
		t.Fatalf("restart repeated purchase, allocation count %d", count)
	}
}

func TestRestartKeepsExistingOrderTokenUsable(t *testing.T) {
	dir := t.TempDir()
	p := &testProvider{}
	a, err := New(testOptions(dir, p))
	if err != nil {
		t.Fatal(err)
	}
	first := redeem(t, a, "DEMO-EMAIL")
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(testOptions(dir, p))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	current := parseOrder(t, apiRequest(reopened.Handler(), http.MethodGet, "/api/orders/current", nil, first.Token, nil, ""))
	if current.Order.ID != first.Order.ID || current.Order.Resource != first.Order.Resource {
		t.Fatal("restart lost the active resource/session")
	}
	if count, _, _, _ := p.counts(); count != 1 {
		t.Fatal("restart allocated a replacement resource")
	}
	invalid := apiRequest(reopened.Handler(), http.MethodGet, "/api/orders/current", nil, first.Token+"x", nil, "")
	if invalid.Code != http.StatusUnauthorized {
		t.Fatalf("forged order token HTTP %d", invalid.Code)
	}
}

func adminCookie(t *testing.T, a *App) *http.Cookie {
	t.Helper()
	w := apiRequest(a.Handler(), http.MethodPost, "/api/admin/login", map[string]string{"password": testAdminPassword}, "", nil, "http://example.test")
	if w.Code != http.StatusOK {
		t.Fatalf("admin login HTTP %d: %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("missing admin cookie")
	}
	return cookies[0]
}

func TestIssuedVoucherUsesLivePhonePolicyRetainsWaitAndEncryptsRecoverableCode(t *testing.T) {
	p := &testProvider{}
	a := newTestApp(t, p)
	h := a.Handler()
	cookie := adminCookie(t, a)
	s := defaultSettings()
	s.PhoneCountry, s.PhoneMaxPrice, s.PhoneTTLMinutes = "187", "2.50", 12
	w := apiRequest(h, http.MethodPut, "/api/admin/settings", s, "", cookie, "http://example.test")
	if w.Code != http.StatusOK {
		t.Fatalf("settings HTTP %d: %s", w.Code, w.Body.String())
	}
	w = apiRequest(h, http.MethodPost, "/api/admin/cdks", map[string]any{"kind": "phone", "quantity": 1, "note": "snapshot test", "expires_days": 7, "max_attempts": 2}, "", cookie, "http://example.test")
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("issuance HTTP %d: %s", w.Code, w.Body.String())
	}
	var issue struct {
		Codes []string `json:"codes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &issue); err != nil {
		t.Fatal(err)
	}
	if len(issue.Codes) != 1 {
		t.Fatalf("issued %d codes", len(issue.Codes))
	}
	code := issue.Codes[0]
	c, err := scanCDK(a.db.QueryRow("SELECT "+cdkColumns+" FROM cdks WHERE hash=?", hash(code)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Hash != hash(code) || c.MaskedCode == code || c.CodeCipher == "" || bytes.Contains([]byte(c.CodeCipher), []byte(code)) {
		t.Fatal("voucher lookup hash or encrypted recovery code missing")
	}
	s.PhoneCountry, s.PhoneMaxPrice, s.PhoneTTLMinutes = "16", "9.99", 20
	s.PhoneService = "tg"
	w = apiRequest(h, http.MethodPut, "/api/admin/settings", s, "", cookie, "http://example.test")
	if w.Code != http.StatusOK {
		t.Fatalf("new settings HTTP %d: %s", w.Code, w.Body.String())
	}
	redeem(t, a, code)
	p.mu.Lock()
	req := p.requests[0]
	p.mu.Unlock()
	if req.Country != "16" || req.MaxPrice != "9.99" || req.TTL != 12*time.Minute || req.Service != "dr" {
		t.Fatalf("issued voucher did not combine current country/price with its original wait: %+v", req)
	}
	list := apiRequest(h, http.MethodGet, "/api/admin/cdks", nil, "", cookie, "")
	if list.Code != http.StatusOK {
		t.Fatalf("voucher list HTTP %d", list.Code)
	}
	if !bytes.Contains(list.Body.Bytes(), []byte(code)) || bytes.Contains(list.Body.Bytes(), []byte(c.Hash)) || bytes.Contains(list.Body.Bytes(), []byte(c.CodeCipher)) {
		t.Fatal("admin listing must reveal the code but never the hash or ciphertext")
	}
}

func TestManualReviewResolutionRequiresAdminAndControlsRetry(t *testing.T) {
	for _, resolution := range []string{"released", "consumed"} {
		t.Run(resolution, func(t *testing.T) {
			p := &testProvider{allocationErrors: []error{&provider.Error{Code: "timeout", Message: "结果待核对", Uncertain: true}}}
			a := newTestApp(t, p)
			first := redeem(t, a, "DEMO-PHONE")
			path := "/api/admin/orders/" + first.Order.ID + "/resolve"
			body := map[string]string{"resolution": resolution}
			w := apiRequest(a.Handler(), http.MethodPost, path, body, "", nil, "http://example.test")
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated resolution HTTP %d", w.Code)
			}
			cookie := adminCookie(t, a)
			w = apiRequest(a.Handler(), http.MethodPost, path, body, "", cookie, "http://example.test")
			if w.Code != http.StatusOK {
				t.Fatalf("resolve HTTP %d: %s", w.Code, w.Body.String())
			}
			resumed := redeem(t, a, "DEMO-PHONE")
			count, _, _, _ := p.counts()
			if resolution == "released" {
				if resumed.Order.ID == first.Order.ID || count != 2 {
					t.Fatal("confirmed release did not permit new allocation")
				}
			} else {
				if resumed.Order.ID != first.Order.ID || resumed.Order.CanRetry || count != 1 {
					t.Fatal("consumed resolution permitted reuse")
				}
				assertCDK(t, a, resumed.Order, "used", 1)
			}
		})
	}
}

func TestRestartNeverReleasesVoucherAfterCodeDelivery(t *testing.T) {
	dir := t.TempDir()
	p := &testProvider{pollResult: provider.Result{Status: "received", Code: "928314"}, completeError: &provider.Error{Code: "network", Message: "完成结果待确认"}}
	a, err := New(testOptions(dir, p))
	if err != nil {
		t.Fatal(err)
	}
	setupTestAdmin(t, a, testAdminPassword)
	first := redeem(t, a, "DEMO-PHONE")
	parseOrder(t, apiRequest(a.Handler(), http.MethodGet, "/api/orders/current", nil, first.Token, nil, ""))
	// The code arrived, but confirming completion remained unavailable until
	// well after expiry. Restarting must preserve the already-spent voucher.
	if _, err = a.db.Exec("UPDATE orders SET expires_at=?,last_poll=0 WHERE id=?", time.Now().Add(-time.Hour).UnixMilli(), first.Order.ID); err != nil {
		a.Close()
		t.Fatal(err)
	}
	review := parseOrder(t, apiRequest(a.Handler(), http.MethodPost, "/api/orders/complete", map[string]any{}, first.Token, nil, ""))
	if review.Order.Status != "review" || review.Order.Code != "928314" {
		a.Close()
		t.Fatalf("completion reconciliation: %+v", review.Order)
	}
	assertCDK(t, a, review.Order, "review", 1)
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(testOptions(dir, p))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	assertCDK(t, reopened, review.Order, "review", 1)
	cookie := adminCookie(t, reopened)
	w := apiRequest(reopened.Handler(), http.MethodPost, "/api/admin/orders/"+first.Order.ID+"/resolve", map[string]string{"resolution": "released"}, "", cookie, "http://example.test")
	if w.Code != http.StatusConflict {
		t.Fatalf("spent voucher release HTTP %d: %s", w.Code, w.Body.String())
	}
	assertCDK(t, reopened, review.Order, "review", 1)
	resumed := redeem(t, reopened, "DEMO-PHONE")
	if resumed.Order.CanRetry || resumed.Order.Code != "928314" {
		t.Fatal("reconciliation lost delivered code or allowed voucher reuse")
	}
	if count, _, _, _ := p.counts(); count != 1 {
		t.Fatal("code-bearing voucher allocated again")
	}
}

func TestDataDirectoryAllowsOnlyOneRunningApp(t *testing.T) {
	dir := t.TempDir()
	p := &testProvider{}
	first, err := New(testOptions(dir, p))
	if err != nil {
		t.Fatal(err)
	}
	order := redeem(t, first, "DEMO-PHONE")
	second, err := New(testOptions(dir, p))
	if err == nil {
		second.Close()
		first.Close()
		t.Fatal("second app acquired an already-running data directory")
	}
	current, err := first.getOrder(order.Order.ID)
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	if current.Status != "waiting" {
		first.Close()
		t.Fatalf("failed second instance changed active order to %s", current.Status)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	// A failed initialization after taking the filesystem lock must also
	// release it; switching demo/live data intentionally fails validation.
	wrongMode := testOptions(dir, p)
	wrongMode.Mode = "live"
	unexpected, err := New(wrongMode)
	if err == nil {
		unexpected.Close()
		t.Fatal("demo data directory accepted live mode")
	}
	reopened, err := New(testOptions(dir, p))
	if err != nil {
		t.Fatalf("instance lock was not released: %v", err)
	}
	defer reopened.Close()
	resumed := redeem(t, reopened, "DEMO-PHONE")
	if resumed.Order.ID != order.Order.ID || resumed.Order.Status != "waiting" {
		t.Fatal("reopening did not preserve the active order")
	}
}
