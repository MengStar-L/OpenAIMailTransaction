package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openai-mail-transaction/internal/updater"
)

type updateRoundTripper func(*http.Request) (*http.Response, error)

func (f updateRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func updateTestApp(t *testing.T) (*App, *updater.Manager, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	var a *App
	manager, err := updater.New(updater.Options{
		Version: "v1.0.0", Repository: "example/shiguang", DataDir: t.TempDir(),
		Shutdown:    func() { t.Error("authentication tests must never install/restart") },
		BeforeApply: func() error { return a.CanUpdate() },
		CancelApply: func() { a.CancelUpdate() },
		HTTPClient: &http.Client{Transport: updateRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			if r.Method != http.MethodGet || r.URL.String() != "https://api.github.com/repos/example/shiguang/releases/latest" {
				t.Errorf("unexpected update operation %s %s", r.Method, r.URL)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"tag_name":"v1.0.0","draft":false,"prerelease":false,"assets":[]}`)), Request: r}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions(t.TempDir(), &testProvider{})
	opts.Updates = manager
	a, err = New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	setupTestAdmin(t, a, testAdminPassword)
	return a, manager, &calls
}

var updateRoutes = []struct {
	method, path string
	body         any
}{
	{http.MethodGet, "/api/admin/updates", nil},
	{http.MethodPut, "/api/admin/updates/settings", map[string]bool{"auto_check": false, "auto_update": false}},
	{http.MethodPost, "/api/admin/updates/check", map[string]any{}},
	{http.MethodPost, "/api/admin/updates/apply", map[string]any{}},
}

func TestUpdatesRequireAdministratorAndRejectVoucherTokens(t *testing.T) {
	a, manager, calls := updateTestApp(t)
	user := redeem(t, a, "DEMO-EMAIL")
	expired := &http.Cookie{Name: "atelier_admin", Value: "expired-update-test-session"}
	a.sessions.Store(hash(expired.Value), time.Now().Add(-time.Minute))
	for _, route := range updateRoutes {
		for _, credentials := range []struct {
			name, token string
			cookie      *http.Cookie
		}{
			{"anonymous", "", nil},
			{"voucher-token", user.Token, nil},
			{"forged-admin", "", &http.Cookie{Name: "atelier_admin", Value: "forged-session"}},
			{"expired-admin", "", expired},
		} {
			t.Run(route.method+route.path+"/"+credentials.name, func(t *testing.T) {
				w := apiRequest(a.Handler(), route.method, route.path, route.body, credentials.token, credentials.cookie, "http://example.test")
				if w.Code != http.StatusUnauthorized {
					t.Fatalf("update route accepted non-administrator: HTTP %d: %s", w.Code, w.Body.String())
				}
				if strings.Contains(w.Body.String(), "example/shiguang") || strings.Contains(w.Body.String(), "v1.0.0") {
					t.Fatal("unauthorized update request exposed administrator metadata")
				}
			})
		}
	}
	if calls.Load() != 0 || !manager.Settings().AutoCheck || manager.Settings().AutoUpdate {
		t.Fatal("unauthorized requests contacted GitHub or changed update preferences")
	}
}

func TestUpdateMutationsRejectCrossOriginEvenWithAdministratorSession(t *testing.T) {
	a, manager, calls := updateTestApp(t)
	cookie := adminCookie(t, a)
	for _, route := range updateRoutes {
		if route.method == http.MethodGet {
			continue
		}
		for _, origin := range []string{"https://attacker.test", "null"} {
			w := apiRequest(a.Handler(), route.method, route.path, route.body, "", cookie, origin)
			if w.Code != http.StatusForbidden {
				t.Fatalf("cross-origin %s %s HTTP %d", route.method, route.path, w.Code)
			}
		}
		data, _ := json.Marshal(route.body)
		r := httptest.NewRequest(route.method, "http://example.test"+route.path, strings.NewReader(string(data)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("cross-site request without Origin reached %s: HTTP %d", route.path, w.Code)
		}
	}
	if calls.Load() != 0 || !manager.Settings().AutoCheck || manager.Settings().AutoUpdate {
		t.Fatal("cross-site requests contacted GitHub or changed preferences")
	}
}

func TestUpdateAdministratorCanInspectConfigureAndCheckWithoutInstalling(t *testing.T) {
	a, manager, calls := updateTestApp(t)
	cookie := adminCookie(t, a)
	w := apiRequest(a.Handler(), http.MethodGet, "/api/admin/updates", nil, "", cookie, "")
	var state updater.State
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &state) != nil || state.CurrentVersion != "v1.0.0" || state.Repository != "example/shiguang" {
		t.Fatalf("administrator update status: HTTP %d %s", w.Code, w.Body.String())
	}
	w = apiRequest(a.Handler(), http.MethodPut, "/api/admin/updates/settings", map[string]bool{"auto_check": false, "auto_update": false}, "", cookie, "http://example.test")
	if w.Code != http.StatusOK || manager.Settings().AutoCheck || manager.Settings().AutoUpdate {
		t.Fatalf("administrator could not save update preferences: HTTP %d", w.Code)
	}
	w = apiRequest(a.Handler(), http.MethodPost, "/api/admin/updates/check", map[string]any{}, "", cookie, "http://example.test")
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &state) != nil || state.LatestVersion != "v1.0.0" || state.Available || calls.Load() != 1 {
		t.Fatalf("administrator version check: HTTP %d %s", w.Code, w.Body.String())
	}
	// Already up to date: authenticated apply is rejected without downloading
	// assets, mutating the executable, or invoking a restart callback.
	w = apiRequest(a.Handler(), http.MethodPost, "/api/admin/updates/apply", map[string]any{}, "", cookie, "http://example.test")
	if w.Code != http.StatusConflict || calls.Load() != 1 {
		t.Fatalf("apply without an available release reached an installer: HTTP %d", w.Code)
	}
}

func TestUpdateUnavailableManagerStillRequiresAdministrator(t *testing.T) {
	a := newTestApp(t, &testProvider{})
	cookie := adminCookie(t, a)
	for _, route := range updateRoutes {
		w := apiRequest(a.Handler(), route.method, route.path, route.body, "", nil, "")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("disabled updater bypassed authentication: HTTP %d", w.Code)
		}
		w = apiRequest(a.Handler(), route.method, route.path, route.body, "", cookie, "http://example.test")
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("disabled updater administrator response: HTTP %d", w.Code)
		}
	}
}

func TestUpdateMaintenanceBlocksPurchasesAndClearsAfterFailure(t *testing.T) {
	a, manager, _ := updateTestApp(t)
	cookie := adminCookie(t, a)
	session := redeem(t, a, "DEMO-EMAIL")
	cancelled := parseOrder(t, apiRequest(a.Handler(), http.MethodPost, "/api/orders/cancel", map[string]any{}, session.Token, nil, ""))
	if cancelled.Order.Status != "cancelled" {
		t.Fatal("fixture mailbox failed to cancel")
	}
	if err := a.CanUpdate(); err != nil {
		t.Fatal(err)
	}
	for _, request := range []struct {
		path, token string
		body        any
		cookie      *http.Cookie
	}{
		{"/api/redeem", "", map[string]string{"cdk": "DEMO-PHONE"}, nil},
		{"/api/orders/replace", session.Token, map[string]any{}, nil},
		{"/api/admin/resources", "", map[string]string{"kind": "email", "request_id": "update-maintenance-request"}, cookie},
	} {
		w := apiRequest(a.Handler(), http.MethodPost, request.path, request.body, request.token, request.cookie, "http://example.test")
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("maintenance admitted a purchase through %s: HTTP %d", request.path, w.Code)
		}
	}
	// A same-version release aborts before asset downloads. The manager must
	// invoke CancelApply so a failed update cannot leave purchases blocked.
	if _, err := manager.Apply(context.Background()); err == nil {
		t.Fatal("same-version fixture unexpectedly installed an update")
	}
	if a.updateMaintenance.Load() {
		t.Fatal("failed update retained the maintenance gate")
	}
	replacement := parseOrder(t, apiRequest(a.Handler(), http.MethodPost, "/api/orders/replace", map[string]any{}, session.Token, nil, ""))
	if replacement.Order.ID == session.Order.ID || replacement.Order.Status != "waiting" {
		t.Fatal("resource allocation did not recover after update failure")
	}
}

func TestUpdateMaintenanceDefersEveryActiveOrderState(t *testing.T) {
	a, manager, calls := updateTestApp(t)
	session := redeem(t, a, "DEMO-EMAIL")
	for _, status := range []string{"queued", "allocating", "waiting", "received", "next_pending", "next_uncertain", "complete_pending", "cancel_pending"} {
		if _, err := a.db.Exec("UPDATE orders SET status=? WHERE id=?", status, session.Order.ID); err != nil {
			t.Fatal(err)
		}
		state, err := manager.Apply(context.Background())
		if err == nil || state.Phase != "deferred" || a.updateMaintenance.Load() || calls.Load() != 0 {
			t.Fatalf("active %s order did not safely defer: state=%+v error=%v", status, state, err)
		}
	}
	if _, err := a.db.Exec("UPDATE orders SET status='cancelled' WHERE id=?", session.Order.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.CanUpdate(); err != nil || !a.updateMaintenance.Load() {
		t.Fatalf("completed activity prevented update admission: %v", err)
	}
	a.CancelUpdate()
}
