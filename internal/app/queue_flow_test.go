package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"openai-mail-transaction/internal/provider"
)

func noMailboxStock() error {
	return &provider.Error{Code: "no_stock", Message: "当前条件下暂无可分配资源，请稍后重试"}
}

func queueDue(t *testing.T, a *App, id string) {
	t.Helper()
	if _, err := a.db.Exec("UPDATE order_allocation_queue SET next_attempt_at=0 WHERE order_id=?", id); err != nil {
		t.Fatal(err)
	}
}

func queueStep(t *testing.T, a *App, id string) Order {
	t.Helper()
	queueDue(t, a, id)
	if err := a.processQueuedOrder(id); err != nil {
		t.Fatal(err)
	}
	o, err := a.getOrder(id)
	if err != nil {
		t.Fatal(err)
	}
	a.decorate(&o)
	return o
}

func TestEmailQueueRetriesSameOrderAndChargesOnlyFirstCode(t *testing.T) {
	f := newQuotaHarness(t, 3)
	f.p.allocationErrors = []error{noMailboxStock(), noMailboxStock()}
	first := redeem(t, f.app, f.code)
	if first.Order.Status != "queued" || first.Order.Resource != "" || first.Order.QueueExpiresAt == nil || first.Order.QueueNextAttemptAt == nil || first.Order.QueueAttempts != 1 {
		t.Fatalf("missing durable queue response: %+v", first.Order)
	}
	if wait := time.Until(*first.Order.QueueExpiresAt); wait < 9*time.Minute || wait > 10*time.Minute+time.Second {
		t.Fatalf("unexpected maximum queue wait: %s", wait)
	}
	quotaAssertUsage(t, f.app, first, 0, 3)
	assertCDK(t, f.app, first.Order, "active", 1)
	for range 3 {
		resumed := redeem(t, f.app, f.code)
		if resumed.Order.ID != first.Order.ID || resumed.Order.Status != "queued" {
			t.Fatalf("duplicate redemption did not resume queue: %+v", resumed.Order)
		}
		if err := f.app.processQueuedOrder(first.Order.ID); err != nil {
			t.Fatal(err)
		}
	}
	if calls, _, _, _ := f.p.counts(); calls != 1 {
		t.Fatalf("queue ignored retry delay: calls=%d", calls)
	}
	// Queue time must not consume the lifetime of a mailbox not yet purchased.
	if _, err := f.app.db.Exec("UPDATE orders SET expires_at=? WHERE id=?", millis(time.Now().Add(-time.Minute)), first.Order.ID); err != nil {
		t.Fatal(err)
	}
	second := queueStep(t, f.app, first.Order.ID)
	if second.Status != "queued" || second.QueueAttempts != 2 {
		t.Fatalf("second shortage lost queue: %+v", second)
	}
	acquired := queueStep(t, f.app, first.Order.ID)
	if acquired.Status != "waiting" || acquired.ID != first.Order.ID || acquired.Resource == "" || time.Until(acquired.ExpiresAt) < 14*time.Minute {
		t.Fatalf("queue purchase did not retain order and fresh resource lifetime: %+v", acquired)
	}
	first.Order = acquired
	quotaAssertUsage(t, f.app, first, 0, 3)
	var orders int
	if err := f.app.db.QueryRow("SELECT COUNT(*) FROM orders WHERE cdk_id=?", acquired.CDKID).Scan(&orders); err != nil || orders != 1 {
		t.Fatalf("queue made extra local orders: %d (%v)", orders, err)
	}
	f.p.setCode("384910")
	first = quotaPoll(t, f.app, first)
	quotaAssertUsage(t, f.app, first, 1, 3)
	if calls, _, _, _ := f.p.counts(); calls != 3 {
		t.Fatalf("unexpected purchase attempts=%d", calls)
	}
}

func TestEmailQueueCancelDoesNotContactProviderOrResumeLater(t *testing.T) {
	for _, source := range []string{"user", "admin"} {
		t.Run(source, func(t *testing.T) {
			p := &testProvider{allocationErrors: []error{noMailboxStock()}}
			a := newTestApp(t, p)
			cookie := adminCookie(t, a)
			var queued orderEnvelope
			if source == "admin" {
				queued = adminAllocate(t, a, cookie, "email", "queue-cancel-admin-key-1")
			} else {
				queued = redeem(t, a, "DEMO-EMAIL")
			}
			// Cancelling an unallocated request is entirely local, even if the
			// provider connection has become unavailable in the meantime.
			a.clientMu.Lock()
			a.client = nil
			a.clientMu.Unlock()
			var cancelled orderEnvelope
			if source == "admin" {
				cancelled = adminAction(t, a, cookie, queued.Order.ID, "cancel", map[string]any{})
			} else {
				cancelled = parseOrder(t, apiRequest(a.Handler(), "POST", "/api/orders/cancel", map[string]any{}, queued.Token, nil, ""))
			}
			if cancelled.Order.Status != "cancelled" || !cancelled.Order.CanRetry {
				t.Fatalf("local queue cancellation failed: %+v", cancelled.Order)
			}
			queueStep(t, a, queued.Order.ID)
			if calls, polls, cancels, _ := p.counts(); calls != 1 || polls != 0 || cancels != 0 {
				t.Fatalf("cancelled unallocated queue contacted provider: calls=%d polls=%d cancels=%d", calls, polls, cancels)
			}
			if source == "admin" {
				resumed := adminAllocate(t, a, cookie, "email", "queue-cancel-admin-key-1")
				if resumed.Order.ID != queued.Order.ID || resumed.Order.Status != "cancelled" {
					t.Fatal("replayed administrator request bought after queue cancellation")
				}
			} else {
				assertCDK(t, a, queued.Order, "available", 0)
			}
		})
	}
}

func TestEmailQueueDeadlineStopsWithoutBuyingAndReleasesQuota(t *testing.T) {
	f := newQuotaHarness(t, 3)
	f.p.allocationErrors = []error{noMailboxStock()}
	queued := redeem(t, f.app, f.code)
	if _, err := f.app.db.Exec("UPDATE order_allocation_queue SET expires_at=? WHERE order_id=?", millis(time.Now().Add(-time.Second)), queued.Order.ID); err != nil {
		t.Fatal(err)
	}
	ended := queueStep(t, f.app, queued.Order.ID)
	if !terminalOrder(ended.Status) || !ended.CanRetry {
		t.Fatalf("expired queue did not release resource slot: %+v", ended)
	}
	queued.Order = ended
	quotaAssertUsage(t, f.app, queued, 0, 3)
	assertCDK(t, f.app, ended, "available", 0)
	if calls, _, cancels, _ := f.p.counts(); calls != 1 || cancels != 0 {
		t.Fatal("expired queue made a paid request or cancelled a nonexistent mailbox")
	}
	if retry := redeem(t, f.app, f.code); retry.Order.Status != "waiting" || retry.Order.ID == ended.ID {
		t.Fatalf("voucher cannot be used after queue timeout: %+v", retry.Order)
	}
}

func TestEmailQueueRestartResumesConfirmedShortageButNeverReplaysPurchaseIntent(t *testing.T) {
	for _, state := range []string{"queued", "allocating"} {
		t.Run(state, func(t *testing.T) {
			f := newQuotaHarness(t, 3)
			f.p.allocationErrors = []error{noMailboxStock()}
			queued := redeem(t, f.app, f.code)
			if state == "allocating" {
				if _, err := f.app.db.Exec("UPDATE orders SET status='allocating' WHERE id=?", queued.Order.ID); err != nil {
					t.Fatal(err)
				}
			}
			f.restart(t)
			resumed := parseOrder(t, apiRequest(f.app.Handler(), "GET", "/api/orders/current", nil, queued.Token, nil, ""))
			want := state
			if state == "allocating" {
				want = "review"
			}
			if resumed.Order.ID != queued.Order.ID || resumed.Order.Status != want {
				t.Fatalf("restart state=%s response=%+v", state, resumed.Order)
			}
			if calls, _, _, _ := f.p.counts(); calls != 1 {
				t.Fatal("restart performed a purchase before a safe scheduled retry")
			}
			advanced := queueStep(t, f.app, queued.Order.ID)
			calls, _, _, _ := f.p.counts()
			if state == "queued" && (advanced.Status != "waiting" || calls != 2) {
				t.Fatalf("durable queue did not resume: %+v calls=%d", advanced, calls)
			}
			if state == "allocating" && (advanced.Status != "review" || calls != 1) {
				t.Fatalf("uncertain purchase was replayed: %+v calls=%d", advanced, calls)
			}
		})
	}
}

func TestEmailQueueOnlyRetriesDefiniteNoStock(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status string
	}{
		{"balance", &provider.Error{Code: "no_balance", Message: "余额不足"}, "failed"},
		{"price", &provider.Error{Code: "price_unavailable", Message: "价格不可用"}, "failed"},
		{"key", &provider.Error{Code: "bad_key", Message: "密钥无效"}, "failed"},
		{"configuration", &provider.Error{Code: "upstream_configuration", Message: "配置无效"}, "failed"},
		{"uncertain_stock", &provider.Error{Code: "no_stock", Message: "result unknown", Uncertain: true}, "review"},
		{"transport", &provider.Error{Code: "upstream_unavailable", Uncertain: true}, "review"},
		{"unknown", errors.New("unexpected provider error"), "review"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &testProvider{allocationErrors: []error{noMailboxStock(), tc.err}}
			a := newTestApp(t, p)
			queued := redeem(t, a, "DEMO-EMAIL")
			ended := queueStep(t, a, queued.Order.ID)
			if ended.Status != tc.status {
				t.Fatalf("non-shortage retry outcome: %s want %s", ended.Status, tc.status)
			}
			queueStep(t, a, queued.Order.ID)
			if calls, _, _, _ := p.counts(); calls != 2 {
				t.Fatalf("unsafe automatic retry after %s: calls=%d", tc.name, calls)
			}
		})
	}
	p := &testProvider{allocationErrors: []error{noMailboxStock()}}
	a := newTestApp(t, p)
	if phone := redeem(t, a, "DEMO-PHONE"); phone.Order.Status != "waiting" || phone.Order.QueueExpiresAt != nil || p.allocations != 2 {
		t.Fatalf("phone did not use its short retry window: %+v", phone.Order)
	}
	var queuedPhones int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM order_allocation_queue q JOIN orders o ON o.id=q.order_id WHERE o.kind='phone'").Scan(&queuedPhones); err != nil || queuedPhones != 0 {
		t.Fatalf("phone entered the mailbox queue: %d %v", queuedPhones, err)
	}
}

func TestEmailQueueUsesOriginalConfigurationAndDoesNotStoreCredentials(t *testing.T) {
	for _, source := range []string{"user", "admin"} {
		t.Run(source, func(t *testing.T) {
			p := &testProvider{allocationErrors: []error{noMailboxStock()}}
			a := newTestApp(t, p)
			a.clientMu.Lock()
			a.providerKey = "queue-private-upstream-key"
			a.clientMu.Unlock()
			cookie := adminCookie(t, a)
			var first orderEnvelope
			if source == "admin" {
				first = adminAllocate(t, a, cookie, "email", "queue-freeze-admin-key-1")
			} else {
				first = redeem(t, a, "DEMO-EMAIL")
			}
			var stored string
			if err := a.db.QueryRow("SELECT request_json FROM order_allocation_queue WHERE order_id=?", first.Order.ID).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(stored, "queue-private-upstream-key") || strings.Contains(stored, "api_key") {
				t.Fatal("queue snapshot stored upstream credentials")
			}
			a.settingsMu.Lock()
			a.settings.EmailService, a.settings.EmailDomain, a.settings.EmailMaxPrice, a.settings.EmailTTLMinutes = "other", "icloud.com", "9.00", 3
			a.settingsMu.Unlock()
			if o := queueStep(t, a, first.Order.ID); o.Status != "waiting" {
				t.Fatalf("configuration edit prevented original request: %+v", o)
			}
			p.mu.Lock()
			requests := append([]provider.Request(nil), p.requests...)
			p.mu.Unlock()
			if len(requests) != 2 || !reflect.DeepEqual(requests[0], requests[1]) {
				t.Fatalf("queue silently switched purchase conditions: %+v", requests)
			}
			w := apiRequest(a.Handler(), "GET", "/api/orders/current", nil, first.Token, nil, "")
			if source == "admin" {
				w = apiRequest(a.Handler(), "GET", "/api/admin/resources/"+first.Order.ID, nil, "", cookie, "")
			}
			if strings.Contains(w.Body.String(), "queue-private-upstream-key") || strings.Contains(w.Body.String(), "request_json") {
				t.Fatal("queue response exposed server purchase snapshot or credentials")
			}
		})
	}
}

func TestEmailQueueDisabledOrExpiredVoucherNeverPurchases(t *testing.T) {
	for _, reason := range []string{"disabled", "expired"} {
		t.Run(reason, func(t *testing.T) {
			p := &testProvider{allocationErrors: []error{noMailboxStock()}}
			a := newTestApp(t, p)
			queued := redeem(t, a, "DEMO-EMAIL")
			o, err := a.getOrder(queued.Order.ID)
			if err != nil {
				t.Fatal(err)
			}
			if reason == "disabled" {
				_, err = a.db.Exec("UPDATE cdks SET status='disabled' WHERE id=?", o.CDKID)
			} else {
				_, err = a.db.Exec("UPDATE cdks SET expires_at=? WHERE id=?", millis(time.Now().Add(-time.Second)), o.CDKID)
			}
			if err != nil {
				t.Fatal(err)
			}
			ended := queueStep(t, a, o.ID)
			if !terminalOrder(ended.Status) {
				t.Fatalf("invalid voucher remained in queue: %+v", ended)
			}
			if calls, _, _, _ := p.counts(); calls != 1 {
				t.Fatalf("%s voucher bought a mailbox", reason)
			}
			c, err := a.getCDK(o.CDKID)
			if err != nil || c.UsedCount != 0 || (reason == "disabled" && c.Status != "disabled") {
				t.Fatalf("invalid voucher state changed: %+v (%v)", c, err)
			}
		})
	}
}

type heldQueueProvider struct {
	*testProvider
	entered, release chan struct{}
}

func (p *heldQueueProvider) Allocate(ctx context.Context, request provider.Request) (provider.Activation, error) {
	activation, err := p.testProvider.Allocate(ctx, request)
	if activation.ID != "" {
		close(p.entered)
		select {
		case <-p.release:
		case <-ctx.Done():
			return provider.Activation{}, &provider.Error{Uncertain: true, Message: "timed out"}
		}
	}
	return activation, err
}

func TestEmailQueueConcurrentRetryCancelAndRequestsPreserveSinglePurchase(t *testing.T) {
	for _, source := range []string{"user", "admin"} {
		t.Run(source, func(t *testing.T) {
			p := &heldQueueProvider{testProvider: &testProvider{allocationErrors: []error{noMailboxStock()}, cancelDelay: time.Minute}, entered: make(chan struct{}), release: make(chan struct{})}
			opts := testOptions(t.TempDir(), p.testProvider)
			opts.Client = p
			a, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			cookie := setupTestAdmin(t, a, testAdminPassword)
			var queued orderEnvelope
			if source == "admin" {
				queued = adminAllocate(t, a, cookie, "email", "queue-race-admin-key-1")
			} else {
				queued = redeem(t, a, "DEMO-EMAIL")
			}
			queueDue(t, a, queued.Order.ID)
			done := make(chan error, 1)
			go func() { done <- a.processQueuedOrder(queued.Order.ID) }()
			select {
			case <-p.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("queued retry never reached provider")
			}
			cancelResult := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				path, token, auth := "/api/orders/cancel", queued.Token, (*http.Cookie)(nil)
				if source == "admin" {
					path, token, auth = "/api/admin/resources/"+queued.Order.ID+"/cancel", "", cookie
				}
				cancelResult <- apiRequest(a.Handler(), "POST", path, map[string]any{}, token, auth, "")
			}()
			responses := make(chan *httptest.ResponseRecorder, 6)
			var wg sync.WaitGroup
			for i := range 6 {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					if source == "admin" {
						responses <- apiRequest(a.Handler(), "POST", "/api/admin/resources", map[string]string{"kind": "email", "request_id": fmt.Sprintf("queue-race-admin-duplicate-%d", i)}, "", cookie, "")
					} else {
						responses <- apiRequest(a.Handler(), "POST", "/api/redeem", map[string]string{"cdk": "DEMO-EMAIL"}, "", nil, "")
					}
				}(i)
			}
			close(p.release)
			select {
			case err = <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("queue purchase deadlocked")
			}
			wg.Wait()
			close(responses)
			for w := range responses {
				resumed := parseOrder(t, w)
				if resumed.Order.ID != queued.Order.ID || resumed.Order.Resource == "" {
					t.Fatalf("duplicate lost the purchased resource: %+v", resumed.Order)
				}
			}
			cancelled := parseOrder(t, <-cancelResult)
			if cancelled.Order.Status != "cancel_pending" || cancelled.Order.CanRetry {
				t.Fatalf("in-flight success lost the deferred cancellation: %+v", cancelled.Order)
			}
			if calls, _, cancels, _ := p.counts(); calls != 2 || cancels != 0 {
				t.Fatalf("concurrent actions duplicated or lost purchase: calls=%d cancels=%d", calls, cancels)
			}
			o, err := a.getOrder(queued.Order.ID)
			if err != nil || o.Status != "cancel_pending" || o.ProviderID == "" || o.Resource == "" {
				t.Fatalf("purchased resource not durably retained: %+v (%v)", o, err)
			}
		})
	}
}

func TestEmailQueueInterruptedSuccessfulPurchaseCannotReplayOnRestart(t *testing.T) {
	f := newQuotaHarness(t, 3)
	f.p.allocationErrors = []error{noMailboxStock()}
	queued := redeem(t, f.app, f.code)
	if bought := queueStep(t, f.app, queued.Order.ID); bought.Status != "waiting" {
		t.Fatalf("local upstream did not allocate: %+v", bought)
	}
	// Restore the durable intent that would be left by a crash after the remote
	// purchase succeeded but before its reply was committed to the local store.
	if _, err := f.app.db.Exec("UPDATE orders SET status='allocating',provider_id='',resource='' WHERE id=?", queued.Order.ID); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	if resumed := queueStep(t, f.app, queued.Order.ID); resumed.Status != "review" || resumed.CanRetry {
		t.Fatalf("unknown charged purchase escaped reconciliation: %+v", resumed)
	}
	if calls, _, _, _ := f.p.counts(); calls != 2 {
		t.Fatalf("unknown charged purchase repeated: calls=%d", calls)
	}
}

func TestEmailQueueAdministratorListsIncludeMetadataWithoutDeadlock(t *testing.T) {
	p := &testProvider{allocationErrors: []error{noMailboxStock(), noMailboxStock()}}
	a := newTestApp(t, p)
	cookie := adminCookie(t, a)
	redeem(t, a, "DEMO-EMAIL")
	adminAllocate(t, a, cookie, "email", "queue-list-admin-key-1")
	for _, path := range []string{"/api/admin/overview", "/api/admin/resources"} {
		response := make(chan *httptest.ResponseRecorder, 1)
		go func() { response <- apiRequest(a.Handler(), "GET", path, nil, "", cookie, "") }()
		select {
		case w := <-response:
			var payload struct {
				Orders []Order `json:"orders"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &payload) != nil || len(payload.Orders) == 0 {
				t.Fatalf("list failed: %s HTTP %d %s", path, w.Code, w.Body.String())
			}
			for _, o := range payload.Orders {
				if o.Status != "queued" || o.QueueExpiresAt == nil || o.QueueNextAttemptAt == nil || o.QueueAttempts != 1 {
					t.Fatalf("list omitted queue metadata: %s %+v", path, o)
				}
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s deadlocked while decorating queue rows", path)
		}
	}
}

func queueMigrationSnapshot(t *testing.T, db *sql.DB) map[string][][]any {
	t.Helper()
	snapshot := map[string][][]any{}
	for _, table := range []string{"orders", "cdks", "admin_resource_requests", "settings", "metadata", "admin_credentials"} {
		rows, err := db.Query("SELECT * FROM " + table + " ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range pointers {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			snapshot[table] = append(snapshot[table], values)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
	}
	return snapshot
}

func TestEmailQueueMigrationPreservesOriginalDataAndAdminUniqueness(t *testing.T) {
	a := newTestApp(t, &testProvider{})
	cookie := adminCookie(t, a)
	redeem(t, a, "DEMO-PHONE")
	direct := adminAllocate(t, a, cookie, "email", "queue-migration-admin-key-1")
	before := queueMigrationSnapshot(t, a.db)
	// Reconstruct the previous schema: no queue table and the administrator
	// unique index protecting every nonterminal direct resource.
	if _, err := a.db.Exec(`DROP TABLE order_allocation_queue;
DROP INDEX admin_resource_active_idx;
CREATE UNIQUE INDEX admin_resource_active_idx ON orders(kind) WHERE cdk_id IS NULL AND status NOT IN ('completed','cancelled','expired','failed');`); err != nil {
		t.Fatal(err)
	}
	if err := migrateAllocationQueue(a.db); err != nil {
		t.Fatal(err)
	}
	if after := queueMigrationSnapshot(t, a.db); !reflect.DeepEqual(before, after) {
		t.Fatal("queue migration altered existing orders, CDKs, request idempotency, settings or credentials")
	}
	if err := migrateAllocationQueue(a.db); err != nil {
		t.Fatalf("repeat queue migration failed: %v", err)
	}
	if _, err := a.db.Exec("UPDATE orders SET status='queued' WHERE id=?", direct.Order.ID); err != nil {
		t.Fatal(err)
	}
	now := millis(time.Now())
	if _, err := a.db.Exec("INSERT INTO orders(id,cdk_id,kind,status,created_at,expires_at,cancel_after,attempt,max_attempts) VALUES('duplicate-queue',NULL,'email','queued',?,?,?,0,0)", now, now, now); err == nil {
		t.Fatal("queue migration permits two active administrator email requests")
	}
	if _, err := a.db.Exec("INSERT INTO order_allocation_queue(order_id,request_json,expires_at,next_attempt_at,attempts) VALUES('missing-order','{}',?,?,1)", now, now); err == nil {
		t.Fatal("queue migration permits orphan purchase intent")
	}
}
