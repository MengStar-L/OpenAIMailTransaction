package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"openai-mail-transaction/internal/provider"
)

func adminAllocate(t *testing.T, a *App, cookie *http.Cookie, kind, requestID string) orderEnvelope {
	t.Helper()
	w := apiRequest(a.Handler(), "POST", "/api/admin/resources", map[string]string{"kind": kind, "request_id": requestID}, "", cookie, "")
	o := parseOrder(t, w)
	if o.Token != "" || o.Order.Source != "admin" || o.Order.RequestID == "" || o.Order.UsageLimit != 0 || o.Order.UsedCount != 0 {
		t.Fatalf("administrator response exposed voucher session/quota: %s", w.Body.String())
	}
	return o
}

func adminPoll(t *testing.T, a *App, cookie *http.Cookie, id string) orderEnvelope {
	t.Helper()
	if _, err := a.db.Exec("UPDATE orders SET last_poll=0 WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	return parseOrder(t, apiRequest(a.Handler(), "GET", "/api/admin/resources/"+id, nil, "", cookie, ""))
}

func adminAction(t *testing.T, a *App, cookie *http.Cookie, id, action string, body any) orderEnvelope {
	t.Helper()
	return parseOrder(t, apiRequest(a.Handler(), "POST", "/api/admin/resources/"+id+"/"+action, body, "", cookie, ""))
}

func TestAdminResourceAuthenticationAndPublicSessionIsolation(t *testing.T) {
	p := &testProvider{}
	a := newTestApp(t, p)
	cookie := adminCookie(t, a)
	for _, path := range []string{"/api/admin/resources", "/api/admin/resources/not-real", "/api/admin/resources/not-real/cancel", "/api/admin/resources/not-real/complete", "/api/admin/resources/not-real/next-code"} {
		for _, method := range []string{"GET", "POST"} {
			w := apiRequest(a.Handler(), method, path, map[string]any{"kind": "email", "request_id": "auth-request-key-0001", "round": 1}, "", nil, "")
			if w.Code != 401 && w.Code != 405 {
				t.Fatalf("unauthenticated %s %s HTTP %d", method, path, w.Code)
			}
		}
	}
	o := adminAllocate(t, a, cookie, "email", "auth-request-key-0001")
	for _, tc := range []struct{ method, path string }{{"GET", "/api/orders/current"}, {"POST", "/api/orders/cancel"}, {"POST", "/api/orders/complete"}, {"POST", "/api/orders/next-code"}, {"POST", "/api/orders/replace"}} {
		w := apiRequest(a.Handler(), tc.method, tc.path, map[string]int{"round": 1}, a.orderToken(o.Order.ID), cookie, "")
		if w.Code != 401 {
			t.Fatalf("public endpoint accepted signed administrator order: %s HTTP %d", tc.path, w.Code)
		}
	}
	user := redeem(t, a, "DEMO-PHONE")
	w := apiRequest(a.Handler(), "GET", "/api/admin/resources/"+user.Order.ID, nil, "", cookie, "")
	if w.Code != 401 {
		t.Fatalf("direct-resource route accepted voucher resource: HTTP %d", w.Code)
	}
	w = apiRequest(a.Handler(), "POST", "/api/admin/resources", map[string]string{"kind": "phone", "request_id": "cross-site-request-key"}, "", cookie, "https://elsewhere.test")
	if w.Code != 403 {
		t.Fatal("cross-origin administrator purchase was accepted")
	}
}

func TestAdminResourceConcurrentIdempotencyAndCancellation(t *testing.T) {
	p := &testProvider{}
	a := newTestApp(t, p)
	cookie := adminCookie(t, a)
	a.settingsMu.Lock()
	a.settings.PhoneEnabled, a.settings.EmailEnabled = false, false
	a.settingsMu.Unlock()
	var before string
	if err := a.db.QueryRow("SELECT group_concat(id||':'||status||':'||attempts||':'||used_count) FROM cdks").Scan(&before); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			responses <- apiRequest(a.Handler(), "POST", "/api/admin/resources", map[string]string{"kind": "email", "request_id": fmt.Sprintf("concurrent-admin-key-%d", i%2)}, "", cookie, "")
		}()
	}
	wg.Wait()
	close(responses)
	var id string
	for w := range responses {
		o := parseOrder(t, w)
		if id != "" && o.Order.ID != id {
			t.Fatal("duplicate administrator allocation")
		}
		id = o.Order.ID
	}
	if allocations, _, _, _ := p.counts(); allocations != 1 {
		t.Fatalf("upstream purchases=%d, want 1", allocations)
	}
	o := adminAction(t, a, cookie, id, "cancel", map[string]any{})
	if o.Order.Status != "cancelled" || !o.Order.CanRetry {
		t.Fatalf("cannot cancel before first code: %+v", o.Order)
	}
	for i := range 2 {
		retry := adminAllocate(t, a, cookie, "email", fmt.Sprintf("concurrent-admin-key-%d", i))
		if retry.Order.ID != id || retry.Order.Status != "cancelled" {
			t.Fatal("delayed retry after cancellation purchased another resource")
		}
	}
	newOrder := adminAllocate(t, a, cookie, "email", "fresh-admin-key-0001")
	if newOrder.Order.ID == id {
		t.Fatal("new request did not purchase a new mailbox")
	}
	phone := adminAllocate(t, a, cookie, "phone", "fresh-phone-key-0001")
	if phone.Order.Kind != "phone" || phone.Order.ID == newOrder.Order.ID {
		t.Fatal("phone and email could not be acquired independently")
	}
	w := apiRequest(a.Handler(), "POST", "/api/admin/resources", map[string]string{"kind": "phone", "request_id": "fresh-admin-key-0001"}, "", cookie, "")
	if w.Code != 409 {
		t.Fatal("request id was reused for a different resource kind")
	}
	var after string
	if err := a.db.QueryRow("SELECT group_concat(id||':'||status||':'||attempts||':'||used_count) FROM cdks").Scan(&after); err != nil || before != after {
		t.Fatalf("administrator action changed CDKs: before %s after %s err %v", before, after, err)
	}
	w = apiRequest(a.Handler(), "GET", "/api/admin/resources", nil, "", cookie, "")
	var list struct {
		Orders []Order `json:"orders"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Orders) != 2 {
		t.Fatalf("latest direct resources: %s", w.Body.String())
	}
}

func TestAdminResourceThreeMailsRestartRecoveryAndNoQuota(t *testing.T) {
	p := &quotaProvider{testProvider: &testProvider{}}
	opts := testOptions(t.TempDir(), p.testProvider)
	opts.Client = p
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if a != nil {
			a.Close()
		}
	}()
	cookie := setupTestAdmin(t, a, testAdminPassword)
	o := adminAllocate(t, a, cookie, "email", "three-mail-admin-key-01")
	for round := 1; round <= 3; round++ {
		if round > 1 {
			o = adminAction(t, a, cookie, o.Order.ID, "next-code", map[string]int{"round": round - 1})
			o = adminAction(t, a, cookie, o.Order.ID, "next-code", map[string]int{"round": round - 1})
		}
		p.setCode(fmt.Sprintf("12345%d", round))
		o = adminPoll(t, a, cookie, o.Order.ID)
		if len(o.Order.Codes) != round || o.Order.Status != "received" || o.Order.UsedCount != 0 || o.Order.UsageLimit != 0 {
			t.Fatalf("round %d: %+v", round, o.Order)
		}
		if round == 1 {
			if err = a.Close(); err != nil {
				t.Fatal(err)
			}
			a, err = New(opts)
			if err != nil {
				t.Fatal(err)
			}
			cookie = adminCookie(t, a)
			recovered := adminAllocate(t, a, cookie, "email", "three-mail-admin-key-01")
			if recovered.Order.ID != o.Order.ID || !reflect.DeepEqual(recovered.Order.Codes, o.Order.Codes) {
				t.Fatal("restart lost the existing mailbox or receipt")
			}
		}
	}
	if o.Order.CanNextCode || p.nextCount() != 2 {
		t.Fatal("third mail cap or next-action idempotency failed")
	}
	w := apiRequest(a.Handler(), "POST", "/api/admin/resources/"+o.Order.ID+"/next-code", map[string]int{"round": 3}, "", cookie, "")
	if w.Code != 409 {
		t.Fatal("allowed a fourth mail")
	}
	p.completeError = &provider.Error{Message: "uncertain", Uncertain: true}
	o = adminAction(t, a, cookie, o.Order.ID, "complete", map[string]any{})
	if o.Order.Status != "complete_pending" {
		t.Fatal("completion was not durably pending")
	}
	blocked := adminAllocate(t, a, cookie, "email", "pending-admin-key-0001")
	if blocked.Order.ID != o.Order.ID {
		t.Fatal("purchased before confirmed completion")
	}
	p.completeError = nil
	advanceCompletionRetry(t, a, o.Order.ID)
	o = adminPoll(t, a, cookie, o.Order.ID)
	if o.Order.Status != "completed" || len(o.Order.Codes) != 3 {
		t.Fatal("completion lost codes")
	}
	var used, count int
	if err = a.db.QueryRow("SELECT COUNT(*),SUM(used_count) FROM cdks").Scan(&count, &used); err != nil || count != 2 || used != 0 {
		t.Fatalf("admin use changed voucher quota: count=%d used=%d err=%v", count, used, err)
	}
}

func TestAdminResourceUnknownPurchaseNeverReplaysAndCanBeResolved(t *testing.T) {
	p := &testProvider{allocationErrors: []error{&provider.Error{Message: "timeout", Uncertain: true}}}
	dir := t.TempDir()
	opts := testOptions(dir, p)
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if a != nil {
			a.Close()
		}
	}()
	cookie := setupTestAdmin(t, a, testAdminPassword)
	o := adminAllocate(t, a, cookie, "phone", "uncertain-admin-key-01")
	if o.Order.Status != "review" {
		t.Fatal("unknown allocation was not held for review")
	}
	if _, err = a.db.Exec("UPDATE orders SET status='allocating' WHERE id=?", o.Order.ID); err != nil {
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = New(opts)
	if err != nil {
		t.Fatal(err)
	}
	cookie = adminCookie(t, a)
	recovered := adminAllocate(t, a, cookie, "phone", "uncertain-admin-key-02")
	if recovered.Order.ID != o.Order.ID || recovered.Order.Status != "review" {
		t.Fatal("crash intent was not retained")
	}
	if allocations, _, _, _ := p.counts(); allocations != 1 {
		t.Fatal("uncertain allocation was repeated")
	}
	w := apiRequest(a.Handler(), "POST", "/api/admin/orders/"+o.Order.ID+"/resolve", map[string]string{"resolution": "released"}, "", cookie, "")
	if w.Code != 200 {
		t.Fatalf("admin resource resolve failed: %s", w.Body.String())
	}
	o = adminAllocate(t, a, cookie, "phone", "uncertain-admin-key-03")
	if o.Order.Status != "waiting" {
		t.Fatal("resolved direct allocation cannot be replaced")
	}
}

func TestAdminResourceMissingKeyAndParametersBlockPurchase(t *testing.T) {
	a, err := New(liveSettingsOptions(t.TempDir(), "http://127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	cookie := setupTestAdmin(t, a, testAdminPassword)
	for _, kind := range []string{"phone", "email"} {
		w := apiRequest(a.Handler(), "POST", "/api/admin/resources", map[string]string{"kind": kind, "request_id": "missing-key-request-" + kind}, "", cookie, "")
		if w.Code != 409 {
			t.Fatalf("missing key allowed %s allocation: %d", kind, w.Code)
		}
	}
	p := &testProvider{}
	a.client = p
	for _, kind := range []string{"phone", "email"} {
		w := apiRequest(a.Handler(), "POST", "/api/admin/resources", map[string]string{"kind": kind, "request_id": "missing-settings-key-" + kind}, "", cookie, "")
		if w.Code != 409 {
			t.Fatalf("missing settings allowed %s allocation: %d", kind, w.Code)
		}
	}
	if allocations, _, _, _ := p.counts(); allocations != 0 {
		t.Fatal("invalid parameters reached provider")
	}
	var count int
	if err = a.db.QueryRow("SELECT COUNT(*) FROM orders").Scan(&count); err != nil || count != 0 {
		t.Fatal("invalid purchase left an intent")
	}
}

func TestAdminResourceKeyRotationWaitsForEveryUnsettledState(t *testing.T) {
	p := &testProvider{}
	opts := testOptions(t.TempDir(), p)
	opts.Mode, opts.APIKey = "live", savedTestAPIKey
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	cookie := setupTestAdmin(t, a, testAdminPassword)
	s := configuredResourceSettings()
	s.PhoneEnabled, s.EmailEnabled = false, false
	saveAPIKey(t, a, cookie, s, nil)
	o := adminAllocate(t, a, cookie, "email", "key-rotation-admin-01")
	newKey := "administrator-new-key-value"
	for _, status := range []string{"queued", "allocating", "waiting", "received", "cancel_pending", "review", "next_pending", "next_uncertain", "complete_pending"} {
		if _, err = a.db.Exec("UPDATE orders SET status=? WHERE id=?", status, o.Order.ID); err != nil {
			t.Fatal(err)
		}
		w := apiRequest(a.Handler(), "PUT", "/api/admin/settings", apiKeySettings(s, &newKey), "", cookie, "")
		if w.Code != 409 || a.currentAPIKey() != savedTestAPIKey {
			t.Fatalf("key changed during administrator %s order: %d", status, w.Code)
		}
	}
	p.mu.Lock()
	requests := append([]provider.Request(nil), p.requests...)
	p.mu.Unlock()
	if len(requests) != 1 || requests[0].Service != s.EmailService || requests[0].Domain != s.EmailDomain || requests[0].MaxPrice != s.EmailMaxPrice || requests[0].TTL != time.Duration(s.EmailTTLMinutes)*time.Minute {
		t.Fatalf("direct allocation ignored configured provider limits: %+v", requests)
	}
	var count int
	if err = a.db.QueryRow("SELECT COUNT(*) FROM cdks").Scan(&count); err != nil || count != 0 {
		t.Fatal("direct live allocation created a voucher")
	}
}

func TestAdminResourceMigrationPreservesOrdersIndexesAndForeignKeys(t *testing.T) {
	dir := legacyVoucherStore(t)
	db, err := sql.Open("sqlite", filepath.Join(dir, "atelier.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE INDEX custom_resource_idx ON orders(resource)"); err != nil {
		t.Fatal(err)
	}
	if err = migrateVoucherUsage(db); err != nil {
		t.Fatal(err)
	}
	const oldCodes = `[{"round":1,"code":"123456","received_at":"2026-10-07T01:02:03Z"},{"round":2,"code":"654321","received_at":"2026-10-07T01:03:03Z"}]`
	if _, err = db.Exec("UPDATE orders SET mail_round=3,codes=?,usage_counted=1,round_wait_seen=1 WHERE id='old-received'", oldCodes); err != nil {
		t.Fatal(err)
	}
	type orderState struct {
		ID, CDK, Kind, Status, Provider, Resource, Code string
		Created, Expires, Cancel                        int64
		Attempt, Maximum                                int
		LastPoll                                        int64
	}
	read := func(db *sql.DB) []orderState {
		rows, err := db.Query("SELECT id,cdk_id,kind,status,provider_id,resource,code,created_at,expires_at,cancel_after,attempt,max_attempts,last_poll FROM orders ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var states []orderState
		for rows.Next() {
			var o orderState
			if err = rows.Scan(&o.ID, &o.CDK, &o.Kind, &o.Status, &o.Provider, &o.Resource, &o.Code, &o.Created, &o.Expires, &o.Cancel, &o.Attempt, &o.Maximum, &o.LastPoll); err != nil {
				t.Fatal(err)
			}
			states = append(states, o)
		}
		return states
	}
	before := read(db)
	db.Close()
	db, err = openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if after := read(db); !reflect.DeepEqual(before, after) {
		t.Fatalf("migration changed original order fields: before=%+v after=%+v", before, after)
	}
	var round int
	var codes string
	var counted, waitSeen bool
	if err = db.QueryRow("SELECT mail_round,codes,usage_counted,round_wait_seen FROM orders WHERE id='old-received'").Scan(&round, &codes, &counted, &waitSeen); err != nil || round != 3 || codes != oldCodes || !counted || !waitSeen {
		t.Fatalf("mail history was changed during nullable migration: %d %s %t %t %v", round, codes, counted, waitSeen, err)
	}
	for _, name := range []string{"order_cdk_idx", "order_status_idx", "custom_resource_idx", "admin_resource_active_idx"} {
		var count int
		if err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", name).Scan(&count); err != nil || count != 1 {
			t.Fatalf("missing index %s", name)
		}
	}
	now := time.Now().UnixMilli()
	if _, err = db.Exec("INSERT INTO orders(id,cdk_id,kind,status,created_at,expires_at,cancel_after,attempt,max_attempts) VALUES('bad','missing','email','waiting',?,?,?,0,0)", now, now, now); err == nil {
		t.Fatal("migration removed voucher foreign key")
	}
	if _, err = db.Exec("INSERT INTO orders(id,cdk_id,kind,status,created_at,expires_at,cancel_after,attempt,max_attempts) VALUES('admin',NULL,'email','waiting',?,?,?,0,0)", now, now, now); err != nil {
		t.Fatalf("nullable administrator insert failed: %v", err)
	}
	if _, err = db.Exec("INSERT INTO orders(id,cdk_id,kind,status,created_at,expires_at,cancel_after,attempt,max_attempts) VALUES('duplicate',NULL,'email','waiting',?,?,?,0,0)", now, now, now); err == nil {
		t.Fatal("allowed two unsettled administrator mailboxes")
	}
}
