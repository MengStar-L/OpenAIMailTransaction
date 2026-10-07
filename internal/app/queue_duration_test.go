package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"openai-mail-transaction/internal/provider"
)

func TestQueueDurationConfigAndValidationBeforePurchase(t *testing.T) {
	p := &testProvider{}
	a := newTestApp(t, p)
	cookie := adminCookie(t, a)
	w := apiRequest(a.Handler(), "GET", "/api/config", nil, "", nil, "")
	var config map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &config); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]float64{"queue_default_minutes": 10, "queue_min_minutes": 1, "queue_max_minutes": 1440} {
		if config[key] != want {
			t.Fatalf("%s=%v want %v", key, config[key], want)
		}
	}
	for _, invalid := range []any{0, -1, 1441, 1.5, "20", true} {
		for _, source := range []string{"user", "admin"} {
			path, auth := "/api/redeem", (*http.Cookie)(nil)
			body := map[string]any{"cdk": "DEMO-EMAIL", "queue_minutes": invalid}
			if source == "admin" {
				path, auth = "/api/admin/resources", cookie
				body = map[string]any{"kind": "email", "request_id": "queue-invalid-duration-1", "queue_minutes": invalid}
			}
			w := apiRequest(a.Handler(), "POST", path, body, "", auth, "")
			if w.Code != 400 {
				t.Fatalf("%s duration %v accepted: %d %s", source, invalid, w.Code, w.Body.String())
			}
		}
	}
	if calls, _, _, _ := p.counts(); calls != 0 {
		t.Fatal("invalid duration contacted provider")
	}
}

func TestQueueDurationCustomAndBoundsForBothSources(t *testing.T) {
	for _, source := range []string{"user", "admin"} {
		t.Run(source, func(t *testing.T) {
			p := &testProvider{allocationErrors: []error{noMailboxStock(), noMailboxStock(), noMailboxStock()}}
			a := newTestApp(t, p)
			cookie := adminCookie(t, a)
			for _, minutes := range []int{1, 23, 1440} {
				path, auth := "/api/redeem", (*http.Cookie)(nil)
				body := map[string]any{"cdk": "DEMO-EMAIL", "queue_minutes": minutes}
				if source == "admin" {
					path, auth = "/api/admin/resources", cookie
					body = map[string]any{"kind": "email", "request_id": fmt.Sprintf("queue-duration-admin-%d", minutes), "queue_minutes": minutes}
				}
				session := parseOrder(t, apiRequest(a.Handler(), "POST", path, body, "", auth, ""))
				if session.Order.Status != "queued" || session.Order.QueueMinutes != minutes || session.Order.QueueExpiresAt == nil {
					t.Fatalf("custom duration not reflected: %+v", session.Order)
				}
				if got := time.Duration(session.Order.QueueExpiresAt.UnixMilli()-session.Order.CreatedAt.UnixMilli()) * time.Millisecond; got != time.Duration(minutes)*time.Minute {
					t.Fatalf("fixed queue duration=%s want %dm", got, minutes)
				}
				// Replaying allocation with a different duration must resume the
				// exact intent rather than quietly updating or allocating again.
				body["queue_minutes"] = 77
				resumed := parseOrder(t, apiRequest(a.Handler(), "POST", path, body, "", auth, ""))
				if resumed.Order.ID != session.Order.ID || resumed.Order.QueueMinutes != minutes {
					t.Fatal("allocation replay changed queue intent")
				}
				cancelPath, token := "/api/orders/cancel", session.Token
				if source == "admin" {
					cancelPath, token = "/api/admin/resources/"+session.Order.ID+"/cancel", ""
				}
				parseOrder(t, apiRequest(a.Handler(), "POST", cancelPath, map[string]any{}, token, auth, ""))
			}
			if calls, _, _, _ := p.counts(); calls != 3 {
				t.Fatalf("custom duration duplicated purchases: %d", calls)
			}
		})
	}
}

func TestQueueDurationUpdateAndReplacementPersistAcrossRestart(t *testing.T) {
	f := newQuotaHarness(t, 3)
	f.p.allocationErrors = []error{noMailboxStock(), noMailboxStock()}
	session := parseOrder(t, apiRequest(f.app.Handler(), "POST", "/api/redeem", map[string]any{"cdk": f.code, "queue_minutes": 17}, "", nil, ""))
	updated := parseOrder(t, apiRequest(f.app.Handler(), "POST", "/api/orders/queue-wait", map[string]int{"queue_minutes": 31}, session.Token, nil, ""))
	if updated.Order.QueueMinutes != 31 || updated.Order.QueueExpiresAt.Sub(updated.Order.CreatedAt) != 31*time.Minute {
		t.Fatalf("wrong updated duration: %+v", updated.Order)
	}
	deadline := *updated.Order.QueueExpiresAt
	for range 2 {
		updated = parseOrder(t, apiRequest(f.app.Handler(), "POST", "/api/orders/queue-wait", map[string]int{"queue_minutes": 31}, session.Token, nil, ""))
		if !updated.Order.QueueExpiresAt.Equal(deadline) {
			t.Fatal("repeat duration update moved deadline")
		}
	}
	f.restart(t)
	updated = parseOrder(t, apiRequest(f.app.Handler(), "GET", "/api/orders/current", nil, session.Token, nil, ""))
	if updated.Order.QueueMinutes != 31 || !updated.Order.QueueExpiresAt.Equal(deadline) {
		t.Fatal("restart changed queue duration")
	}
	parseOrder(t, apiRequest(f.app.Handler(), "POST", "/api/orders/cancel", map[string]any{}, session.Token, nil, ""))
	replacement := parseOrder(t, apiRequest(f.app.Handler(), "POST", "/api/orders/replace", map[string]int{"queue_minutes": 19}, session.Token, nil, ""))
	if replacement.Order.ID == session.Order.ID || replacement.Order.QueueMinutes != 19 || replacement.Order.QueueExpiresAt.UnixMilli()-replacement.Order.CreatedAt.UnixMilli() != (19*time.Minute).Milliseconds() {
		t.Fatal("replacement ignored new queue duration")
	}
	old := apiRequest(f.app.Handler(), "POST", "/api/orders/queue-wait", map[string]int{"queue_minutes": 40}, session.Token, nil, "")
	if old.Code != 409 {
		t.Fatal("old order token changed a replacement queue")
	}
	if calls, _, _, _ := f.p.counts(); calls != 2 {
		t.Fatal("duration updates contacted provider")
	}
}

func TestQueueDurationShorteningExpiresAtomicallyWithoutPurchase(t *testing.T) {
	f := newQuotaHarness(t, 3)
	f.p.allocationErrors = []error{noMailboxStock()}
	session := redeem(t, f.app, f.code)
	created := time.Now().Add(-2 * time.Minute).UTC()
	if _, err := f.app.db.Exec("UPDATE orders SET created_at=? WHERE id=?", millis(created), session.Order.ID); err != nil {
		t.Fatal(err)
	}
	before, err := f.app.getAllocationQueue(session.Order.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Fail the voucher update inside the expiration transaction: neither the
	// order nor its duration may become half-applied.
	if _, err = f.app.db.Exec(`CREATE TRIGGER queue_expiry_test BEFORE UPDATE ON cdks BEGIN SELECT RAISE(ABORT, 'test rollback'); END`); err != nil {
		t.Fatal(err)
	}
	failed := apiRequest(f.app.Handler(), "POST", "/api/orders/queue-wait", map[string]int{"queue_minutes": 1}, session.Token, nil, "")
	if failed.Code != 500 {
		t.Fatalf("expected atomic rollback, HTTP %d", failed.Code)
	}
	after, err := f.app.getAllocationQueue(session.Order.ID)
	if err != nil || !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatal("failed shortening changed durable deadline")
	}
	stored, err := f.app.getOrder(session.Order.ID)
	if err != nil || stored.Status != "queued" {
		t.Fatal("failed shortening changed order")
	}
	if _, err = f.app.db.Exec("DROP TRIGGER queue_expiry_test"); err != nil {
		t.Fatal(err)
	}
	ended := parseOrder(t, apiRequest(f.app.Handler(), "POST", "/api/orders/queue-wait", map[string]int{"queue_minutes": 1}, session.Token, nil, ""))
	if ended.Order.Status != "expired" || !ended.Order.CanRetry || ended.Order.QueueMinutes != 1 {
		t.Fatalf("shortened queue not expired: %+v", ended.Order)
	}
	quotaAssertUsage(t, f.app, ended, 0, 3)
	assertCDK(t, f.app, ended.Order, "available", 0)
	queueStep(t, f.app, session.Order.ID)
	if calls, _, cancels, _ := f.p.counts(); calls != 1 || cancels != 0 {
		t.Fatal("shortening bought or cancelled upstream resource")
	}
}

func TestQueueDurationExpiredQueueCannotBeRevivedAndLegacyDeadlineIsPreserved(t *testing.T) {
	f := newQuotaHarness(t, 3)
	f.p.allocationErrors = []error{noMailboxStock()}
	session := redeem(t, f.app, f.code)
	// Existing releases used five minutes. Starting this release must leave
	// that durable deadline unchanged, including in the public response.
	legacy := session.Order.CreatedAt.Add(5 * time.Minute)
	if _, err := f.app.db.Exec("UPDATE order_allocation_queue SET expires_at=? WHERE order_id=?", millis(legacy), session.Order.ID); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	resumed := parseOrder(t, apiRequest(f.app.Handler(), "GET", "/api/orders/current", nil, session.Token, nil, ""))
	if resumed.Order.QueueMinutes != 5 || resumed.Order.QueueExpiresAt.UnixMilli() != legacy.UnixMilli() {
		t.Fatal("upgrade silently extended legacy queue")
	}
	past := time.Now().Add(-time.Second)
	if _, err := f.app.db.Exec("UPDATE order_allocation_queue SET expires_at=? WHERE order_id=?", millis(past), session.Order.ID); err != nil {
		t.Fatal(err)
	}
	ended := parseOrder(t, apiRequest(f.app.Handler(), "POST", "/api/orders/queue-wait", map[string]int{"queue_minutes": 60}, session.Token, nil, ""))
	if ended.Order.Status != "expired" || ended.Order.QueueExpiresAt.UnixMilli() != past.UnixMilli() {
		t.Fatal("expired queue revived")
	}
	if calls, _, _, _ := f.p.counts(); calls != 1 {
		t.Fatal("expired queue called provider")
	}
}

func TestQueueDurationUpdateAuthorizationAndAdminSupport(t *testing.T) {
	p := &testProvider{allocationErrors: []error{noMailboxStock(), noMailboxStock()}}
	a := newTestApp(t, p)
	cookie := adminCookie(t, a)
	user := redeem(t, a, "DEMO-EMAIL")
	admin := adminAllocate(t, a, cookie, "email", "queue-duration-auth-admin")
	path := "/api/admin/resources/" + admin.Order.ID + "/queue-wait"
	updated := parseOrder(t, apiRequest(a.Handler(), "POST", path, map[string]int{"queue_minutes": 27}, "", cookie, ""))
	if updated.Order.QueueMinutes != 27 {
		t.Fatal("administrator cannot edit queue wait")
	}
	for _, tc := range []struct {
		path, token string
		cookie      *http.Cookie
		want        int
	}{
		{"/api/orders/queue-wait", "", nil, 401},
		{path, user.Token, nil, 401},
		{"/api/admin/resources/" + user.Order.ID + "/queue-wait", "", cookie, 401},
	} {
		w := apiRequest(a.Handler(), "POST", tc.path, map[string]int{"queue_minutes": 99}, tc.token, tc.cookie, "")
		if w.Code != tc.want {
			t.Fatalf("authorization %s HTTP %d want %d", tc.path, w.Code, tc.want)
		}
	}
	for _, body := range []any{map[string]any{}, map[string]any{"queue_minutes": nil}, map[string]int{"queue_minutes": 0}, map[string]int{"queue_minutes": 1441}} {
		if w := apiRequest(a.Handler(), "POST", "/api/orders/queue-wait", body, user.Token, nil, ""); w.Code != 400 {
			t.Fatalf("invalid queue edit accepted: %d", w.Code)
		}
	}
	phone := redeem(t, a, "DEMO-PHONE")
	if w := apiRequest(a.Handler(), "POST", "/api/orders/queue-wait", map[string]int{"queue_minutes": 10}, phone.Token, nil, ""); w.Code != 409 {
		t.Fatal("phone accepted email queue edit")
	}
}

func TestQueueDurationEditSerializesWithInflightPurchase(t *testing.T) {
	p := &heldQueueProvider{testProvider: &testProvider{allocationErrors: []error{noMailboxStock()}}, entered: make(chan struct{}), release: make(chan struct{})}
	opts := testOptions(t.TempDir(), p.testProvider)
	opts.Client = p
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	session := redeem(t, a, "DEMO-EMAIL")
	queueDue(t, a, session.Order.ID)
	done := make(chan error, 1)
	go func() { done <- a.processQueuedOrder(session.Order.ID) }()
	select {
	case <-p.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("purchase never started")
	}
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- apiRequest(a.Handler(), "POST", "/api/orders/queue-wait", map[string]int{"queue_minutes": 1}, session.Token, nil, "")
	}()
	close(p.release)
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("purchase deadlocked")
	}
	select {
	case w := <-response:
		if w.Code != 409 {
			t.Fatalf("duration edit discarded in-flight purchase: %d %s", w.Code, w.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("duration edit deadlocked")
	}
	o, err := a.getOrder(session.Order.ID)
	if err != nil || o.Status != "waiting" || o.ProviderID == "" || o.Resource == "" {
		t.Fatal("purchased resource lost after duration race")
	}
	if calls, _, cancels, _ := p.counts(); calls != 2 || cancels != 0 {
		t.Fatal("duration race duplicated purchase")
	}
}

type localWaitProvider struct {
	*testProvider
	providerTTL time.Duration
}

func (p *localWaitProvider) Allocate(ctx context.Context, r provider.Request) (provider.Activation, error) {
	v, err := p.testProvider.Allocate(ctx, r)
	v.ExpiresAt = time.Time{}
	if p.providerTTL > 0 {
		v.ExpiresAt = time.Now().Add(p.providerTTL)
	}
	return v, err
}

func TestLocalWaitingDefaultsToTwentyFiveMinutesForMailAndPreservesProviderCap(t *testing.T) {
	for _, ttl := range []time.Duration{0, 3 * time.Minute} {
		t.Run(ttl.String(), func(t *testing.T) {
			p := &localWaitProvider{testProvider: &testProvider{}, providerTTL: ttl}
			opts := testOptions(t.TempDir(), p.testProvider)
			opts.Client = p
			a, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			for _, code := range []string{"DEMO-EMAIL", "DEMO-PHONE"} {
				o := redeem(t, a, code).Order
				want := 20 * time.Minute
				if o.Kind == "email" {
					want = 25 * time.Minute
				}
				if ttl != 0 {
					want = ttl
				}
				if left := time.Until(o.ExpiresAt); left < want-time.Second || left > want+time.Second {
					t.Fatalf("local/provider wait=%s want %s", left, want)
				}
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			for _, r := range p.requests {
				want := 20 * time.Minute
				if r.Kind == "email" {
					want = 25 * time.Minute
				}
				if r.TTL != want {
					t.Fatalf("requested local TTL=%s", r.TTL)
				}
			}
		})
	}
}
