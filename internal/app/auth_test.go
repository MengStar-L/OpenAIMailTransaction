package app

import (
	"bytes"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func setupTestAdmin(t *testing.T, a *App, password string) *http.Cookie {
	t.Helper()
	w := apiRequest(a.Handler(), http.MethodPost, "/api/admin/setup", map[string]string{"password": password, "confirm_password": password}, "", nil, "http://example.test")
	if w.Code != http.StatusOK {
		t.Fatalf("admin setup HTTP %d: %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "atelier_admin" {
		t.Fatalf("admin setup missing session cookie: %v", cookies)
	}
	return cookies[0]
}

func freshTestApp(t *testing.T) *App {
	t.Helper()
	a, err := New(testOptions(t.TempDir(), &testProvider{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	return a
}

func assertSetupRequired(t *testing.T, a *App, want bool) {
	t.Helper()
	w := apiRequest(a.Handler(), http.MethodGet, "/api/admin/bootstrap", nil, "", nil, "")
	var got struct {
		SetupRequired bool `json:"setup_required"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.SetupRequired != want {
		t.Fatalf("bootstrap = HTTP %d %s, want setup_required=%t", w.Code, w.Body.String(), want)
	}
}

func TestFirstAdminSetupPersistsAcrossRestart(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "environment-password-must-be-ignored")
	dir := t.TempDir()
	opts := testOptions(dir, &testProvider{})
	opts.SecureCookies = true
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if a != nil {
			a.Close()
		}
	})
	assertSetupRequired(t, a, true)
	if w := apiRequest(a.Handler(), http.MethodPost, "/api/admin/login", map[string]string{"password": testAdminPassword}, "", nil, "http://example.test"); w.Code != http.StatusConflict {
		t.Fatalf("login before setup HTTP %d: %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/api/admin/session", "/api/admin/overview", "/api/admin/settings", "/api/admin/cdks"} {
		if w := apiRequest(a.Handler(), http.MethodGet, path, nil, "", nil, ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("uninitialized admin data %s HTTP %d", path, w.Code)
		}
	}
	cookie := setupTestAdmin(t, a, testAdminPassword)
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/api/admin" {
		t.Fatalf("unsafe setup cookie: %+v", cookie)
	}
	if w := apiRequest(a.Handler(), http.MethodGet, "/api/admin/session", nil, "", cookie, ""); w.Code != http.StatusOK {
		t.Fatalf("setup session unusable: HTTP %d", w.Code)
	}
	assertSetupRequired(t, a, false)
	var salt, digest []byte
	if err = a.db.QueryRow("SELECT salt,hash FROM admin_credentials WHERE id=1").Scan(&salt, &digest); err != nil {
		t.Fatal(err)
	}
	if len(salt) != 16 || len(digest) != 32 || bytes.Contains(digest, []byte(testAdminPassword)) {
		t.Fatal("administrator password was not stored as a salted hash")
	}
	if _, err = os.Stat(filepath.Join(dir, "admin.json")); !os.IsNotExist(err) {
		t.Fatalf("new setup unexpectedly created a password file: %v", err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a = nil
	a, err = New(opts)
	if err != nil {
		t.Fatal(err)
	}
	assertSetupRequired(t, a, false)
	if w := apiRequest(a.Handler(), http.MethodGet, "/api/admin/session", nil, "", cookie, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("pre-restart session remained valid: HTTP %d", w.Code)
	}
	if w := apiRequest(a.Handler(), http.MethodPost, "/api/admin/setup", map[string]string{"password": "replacement-password", "confirm_password": "replacement-password"}, "", nil, "http://example.test"); w.Code != http.StatusConflict {
		t.Fatalf("repeat setup HTTP %d: %s", w.Code, w.Body.String())
	}
	for password, want := range map[string]int{testAdminPassword: http.StatusOK, "replacement-password": http.StatusUnauthorized, "environment-password-must-be-ignored": http.StatusUnauthorized} {
		w := apiRequest(a.Handler(), http.MethodPost, "/api/admin/login", map[string]string{"password": password}, "", nil, "http://example.test")
		if w.Code != want {
			t.Fatalf("persistent login HTTP %d, want %d", w.Code, want)
		}
	}
}

func TestAdminSetupValidatesPasswordBeforeClaimingAdministrator(t *testing.T) {
	for _, tc := range []struct {
		name, password, confirmation string
	}{
		{"missing", "", ""},
		{"short", "12345678901", "12345678901"},
		{"too long", strings.Repeat("a", 129), strings.Repeat("a", 129)},
		{"blank", strings.Repeat(" ", 12), strings.Repeat(" ", 12)},
		{"mismatch", testAdminPassword, "another-valid-password"},
		{"confirmation missing", testAdminPassword, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := freshTestApp(t)
			w := apiRequest(a.Handler(), http.MethodPost, "/api/admin/setup", map[string]string{"password": tc.password, "confirm_password": tc.confirmation}, "", nil, "http://example.test")
			if w.Code != http.StatusBadRequest || len(w.Result().Cookies()) != 0 {
				t.Fatalf("invalid setup HTTP %d: %s", w.Code, w.Body.String())
			}
			assertSetupRequired(t, a, true)
		})
	}
	for _, password := range []string{strings.Repeat("字", 12), strings.Repeat("字", 128), " password-with-spaces "} {
		t.Run("accepts Unicode and exact whitespace", func(t *testing.T) {
			a := freshTestApp(t)
			setupTestAdmin(t, a, password)
			w := apiRequest(a.Handler(), http.MethodPost, "/api/admin/login", map[string]string{"password": password}, "", nil, "http://example.test")
			if w.Code != http.StatusOK {
				t.Fatalf("valid password login HTTP %d: %s", w.Code, w.Body.String())
			}
			if strings.TrimSpace(password) != password {
				w = apiRequest(a.Handler(), http.MethodPost, "/api/admin/login", map[string]string{"password": strings.TrimSpace(password)}, "", nil, "http://example.test")
				if w.Code != http.StatusUnauthorized {
					t.Fatal("password whitespace was silently removed")
				}
			}
		})
	}
}

func TestConcurrentAdminSetupHasExactlyOneWinner(t *testing.T) {
	a := freshTestApp(t)
	h := a.Handler()
	passwords := []string{"first-browser-password", "second-browser-password"}
	responses := make([]*httptest.ResponseRecorder, len(passwords))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, password := range passwords {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			responses[i] = apiRequest(h, http.MethodPost, "/api/admin/setup", map[string]string{"password": password, "confirm_password": password}, "", nil, "http://example.test")
		}()
	}
	close(start)
	wg.Wait()
	winners := 0
	for i, w := range responses {
		wantLogin := http.StatusUnauthorized
		switch w.Code {
		case http.StatusOK:
			winners++
			wantLogin = http.StatusOK
		case http.StatusConflict:
			if len(w.Result().Cookies()) != 0 {
				t.Fatal("losing setup request received an admin session")
			}
		default:
			t.Fatalf("concurrent setup HTTP %d: %s", w.Code, w.Body.String())
		}
		login := apiRequest(h, http.MethodPost, "/api/admin/login", map[string]string{"password": passwords[i]}, "", nil, "http://example.test")
		if login.Code != wantLogin {
			t.Fatalf("setup winner password was overwritten: login HTTP %d, want %d", login.Code, wantLogin)
		}
	}
	if winners != 1 {
		t.Fatalf("setup had %d successful claimants, want 1", winners)
	}
}

func TestAdminSetupRejectsCrossSiteRequests(t *testing.T) {
	a := freshTestApp(t)
	body := map[string]string{"password": testAdminPassword, "confirm_password": testAdminPassword}
	w := apiRequest(a.Handler(), http.MethodPost, "/api/admin/setup", body, "", nil, "https://attacker.test")
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin setup HTTP %d", w.Code)
	}
	payload, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "http://example.test/api/admin/setup", bytes.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-site setup without Origin HTTP %d", w.Code)
	}
	assertSetupRequired(t, a, true)
	setupTestAdmin(t, a, testAdminPassword)
}

func TestLegacyAdminCredentialsPreserveExistingAdministrator(t *testing.T) {
	dir := t.TempDir()
	salt := []byte("legacy-salt-1234")
	digest, err := pbkdf2.Key(sha256.New, testAdminPassword, salt, 210000, 32)
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := json.Marshal(struct{ Salt, Hash []byte }{Salt: salt, Hash: digest})
	if err = os.WriteFile(filepath.Join(dir, "admin.json"), saved, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := New(testOptions(dir, &testProvider{}))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	assertSetupRequired(t, a, false)
	adminCookie(t, a)
	w := apiRequest(a.Handler(), http.MethodPost, "/api/admin/setup", map[string]string{"password": "replacement-password", "confirm_password": "replacement-password"}, "", nil, "http://example.test")
	if w.Code != http.StatusConflict {
		t.Fatalf("legacy administrator could be overwritten: HTTP %d", w.Code)
	}
}

func TestLiveWithoutAPIKeyAllowsSetupButBlocksResources(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected upstream call", http.StatusInternalServerError)
	}))
	defer upstream.Close()
	opts := testOptions(t.TempDir(), nil)
	opts.Mode, opts.APIBase, opts.Client = "live", upstream.URL, nil
	a, err := New(opts)
	if err != nil {
		t.Fatalf("unconfigured live service failed to start: %v", err)
	}
	defer a.Close()
	cookie := setupTestAdmin(t, a, testAdminPassword)
	w := apiRequest(a.Handler(), http.MethodGet, "/api/config", nil, "", nil, "")
	var config struct {
		Mode       string `json:"mode"`
		Configured bool   `json:"configured"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &config) != nil || config.Mode != "live" || config.Configured {
		t.Fatalf("unconfigured service pretended to be ready: %s", w.Body.String())
	}
	var count int
	if err = a.db.QueryRow("SELECT COUNT(*) FROM cdks").Scan(&count); err != nil || count != 0 {
		t.Fatalf("live service seeded demo vouchers: count=%d error=%v", count, err)
	}
	w = apiRequest(a.Handler(), http.MethodPost, "/api/admin/cdks", map[string]any{"kind": "phone", "quantity": 1, "expires_days": 7, "max_attempts": 2}, "", cookie, "http://example.test")
	if w.Code != http.StatusConflict {
		t.Fatalf("unconfigured live issuance HTTP %d: %s", w.Code, w.Body.String())
	}
	// Supply a pre-existing test voucher to exercise redemption readiness as well.
	if err = a.seedDemo(); err != nil {
		t.Fatal(err)
	}
	w = apiRequest(a.Handler(), http.MethodPost, "/api/redeem", map[string]string{"cdk": "DEMO-PHONE"}, "", nil, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("unconfigured live redemption HTTP %d: %s", w.Code, w.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("unconfigured service made %d upstream requests", calls.Load())
	}
	if err = a.db.QueryRow("SELECT COUNT(*) FROM orders").Scan(&count); err != nil || count != 0 {
		t.Fatalf("unconfigured service allocated an order: count=%d error=%v", count, err)
	}
}
