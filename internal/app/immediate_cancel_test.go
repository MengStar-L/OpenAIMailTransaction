package app

import (
	"net/http"
	"testing"
	"time"

	"openai-mail-transaction/internal/provider"
)

func TestEarlyCancelSurvivesRestartAndReleasesWithoutBrowser(t *testing.T) {
	dir := t.TempDir()
	p := &testProvider{cancelDelay: time.Minute}
	a, err := New(testOptions(dir, p))
	if err != nil {
		t.Fatal(err)
	}
	first := redeem(t, a, "DEMO-PHONE")
	parseOrder(t, apiRequest(a.Handler(), http.MethodPost, "/api/orders/cancel", map[string]any{}, first.Token, nil, ""))
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(testOptions(dir, p))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	resumed := redeem(t, reopened, "DEMO-PHONE")
	if resumed.Order.ID != first.Order.ID || resumed.Order.Status != "cancel_pending" || resumed.Order.CanRetry {
		t.Fatalf("restart lost the pending cancellation: %+v", resumed.Order)
	}
	assertCDK(t, reopened, resumed.Order, "active", 1)
	if _, err = reopened.db.Exec("UPDATE orders SET cancel_after=?,last_poll=0 WHERE id=?", time.Now().Add(-time.Second).UnixMilli(), first.Order.ID); err != nil {
		t.Fatal(err)
	}
	// Exercise the same background path used after the browser is closed.
	if err = reopened.processActiveOrder(first.Order.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := reopened.getOrder(first.Order.ID)
	if err != nil || stored.Status != "cancelled" || stored.UsageCounted {
		t.Fatalf("background cancellation did not finish: %+v, err %v", stored, err)
	}
	assertCDK(t, reopened, stored, "available", 1)
	if allocations, _, cancels, _ := p.counts(); allocations != 1 || cancels != 1 {
		t.Fatalf("unexpected provider calls: allocations=%d, cancellations=%d", allocations, cancels)
	}
}

func TestLateCodeWinsWhileEarlyCancellationIsPending(t *testing.T) {
	p := &testProvider{cancelDelay: time.Minute}
	a := newTestApp(t, p)
	first := redeem(t, a, "DEMO-PHONE")
	parseOrder(t, apiRequest(a.Handler(), http.MethodPost, "/api/orders/cancel", map[string]any{}, first.Token, nil, ""))
	p.mu.Lock()
	p.pollResult = provider.Result{Status: "received", Code: "012345"}
	p.mu.Unlock()
	if _, err := a.db.Exec("UPDATE orders SET last_poll=0 WHERE id=?", first.Order.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.processActiveOrder(first.Order.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := a.getOrder(first.Order.ID)
	if err != nil || stored.Status != "received" || stored.Code != "012345" || !stored.UsageCounted {
		t.Fatalf("late code was lost during the cancellation window: %+v, err %v", stored, err)
	}
	assertCDK(t, a, stored, "active", 1)
	cdk, err := a.getCDK(stored.CDKID)
	if err != nil || cdk.UsedCount != 1 {
		t.Fatalf("late code did not spend exactly one use: %+v, err %v", cdk, err)
	}
	if _, _, cancels, _ := p.counts(); cancels != 0 {
		t.Fatal("late code was cancelled upstream")
	}
}

func TestEarlyCancelDenialRetriesWithoutReleasingQuota(t *testing.T) {
	p := &testProvider{cancelError: &provider.Error{Code: "early_cancel", Message: "上游暂不允许释放", RetryAfter: time.Minute}}
	a := newTestApp(t, p)
	first := redeem(t, a, "DEMO-PHONE")
	before := time.Now()
	cancelled := parseOrder(t, apiRequest(a.Handler(), http.MethodPost, "/api/orders/cancel", map[string]any{}, first.Token, nil, ""))
	if cancelled.Order.Status != "cancel_pending" || cancelled.Order.CanRetry || cancelled.Order.CancelAfter.Before(before.Add(time.Minute-time.Second)) {
		t.Fatalf("upstream cancellation denial lost retry state: %+v", cancelled.Order)
	}
	assertCDK(t, a, first.Order, "active", 1)
	if _, err := a.db.Exec("UPDATE orders SET last_poll=0 WHERE id=?", first.Order.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.processActiveOrder(first.Order.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, cancels, _ := p.counts(); cancels != 1 {
		t.Fatal("upstream retry interval was ignored")
	}
	p.mu.Lock()
	p.cancelError = nil
	p.mu.Unlock()
	if _, err := a.db.Exec("UPDATE orders SET cancel_after=?,last_poll=0 WHERE id=?", time.Now().Add(-time.Second).UnixMilli(), first.Order.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.processActiveOrder(first.Order.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := a.getOrder(first.Order.ID)
	if err != nil || stored.Status != "cancelled" || stored.UsageCounted {
		t.Fatalf("delayed provider confirmation did not release the order: %+v, err %v", stored, err)
	}
	assertCDK(t, a, stored, "available", 1)
	if _, _, cancels, _ := p.counts(); cancels != 2 {
		t.Fatalf("cancellation attempts=%d, want 2", cancels)
	}
}

func TestAdminAcceptsEarlyCancellation(t *testing.T) {
	p := &testProvider{cancelDelay: time.Minute}
	a := newTestApp(t, p)
	cookie := adminCookie(t, a)
	first := adminAllocate(t, a, cookie, "phone", "immediate-admin-cancel-1")
	cancelled := adminAction(t, a, cookie, first.Order.ID, "cancel", map[string]any{})
	if cancelled.Order.Status != "cancel_pending" || cancelled.Order.CanRetry || cancelled.Order.Source != "admin" {
		t.Fatalf("admin cancellation was not accepted: %+v", cancelled.Order)
	}
	if _, _, cancels, _ := p.counts(); cancels != 0 {
		t.Fatal("early admin cancellation ignored the provider window")
	}
}
