package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const savedTestAPIKey = "saved-smsbower-api-key-2026"

func serveSettingsTestCatalog(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Query().Get("action") {
	case "getCountries":
		_, _ = io.WriteString(w, `{"0":{"id":0,"chn":"俄罗斯","eng":"Russia"}}`)
	case "getPricesV3":
		_, _ = io.WriteString(w, `{"0":{"dr":{"2368":{"price":0.1,"count":20,"provider_id":2368}}}}`)
	case "getTopCountriesByService":
		_, _ = io.WriteString(w, `{}`)
	default:
		return false
	}
	return true
}

func redeemSettingsPhone(t *testing.T, a *App, code string) orderEnvelope {
	t.Helper()
	return parseOrder(t, apiRequest(a.Handler(), "POST", "/api/redeem", map[string]string{"cdk": code, "phone_country": "0", "phone_provider_id": "2368"}, "", nil, ""))
}

func liveSettingsOptions(dir, upstream string) Options {
	return Options{DataDir: dir, Mode: "live", APIBase: upstream, DisableWorker: true, Logger: log.New(io.Discard, "", 0)}
}

func apiKeySettings(s Settings, key *string) map[string]any {
	b, _ := json.Marshal(s)
	var payload map[string]any
	_ = json.Unmarshal(b, &payload)
	if key != nil {
		payload["api_key"] = *key
	}
	return payload
}

func configuredResourceSettings() Settings {
	s := defaultSettings()
	s.PhoneCountry, s.PhoneMaxPrice, s.EmailMaxPrice = "0", "1.25", "0.25"
	return s
}

func saveAPIKey(t *testing.T, a *App, cookie *http.Cookie, s Settings, key *string) *httptest.ResponseRecorder {
	t.Helper()
	w := apiRequest(a.Handler(), http.MethodPut, "/api/admin/settings", apiKeySettings(s, key), "", cookie, "http://example.test")
	if w.Code != http.StatusOK {
		t.Fatalf("save settings HTTP %d: %s", w.Code, w.Body.String())
	}
	return w
}

func encryptedAPIKey(t *testing.T, a *App) string {
	t.Helper()
	var encrypted string
	if err := a.db.QueryRow("SELECT value FROM metadata WHERE key='smsbower_api_key_v1'").Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	return encrypted
}

func issueSettingsTestCDK(t *testing.T, a *App, cookie *http.Cookie) string {
	t.Helper()
	w := apiRequest(a.Handler(), http.MethodPost, "/api/admin/cdks", map[string]any{"kind": "phone", "quantity": 1, "expires_days": 7, "max_attempts": 2}, "", cookie, "http://example.test")
	var batch struct {
		Codes []string `json:"codes"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &batch) != nil || len(batch.Codes) != 1 {
		t.Fatalf("issue CDK HTTP %d: %s", w.Code, w.Body.String())
	}
	return batch.Codes[0]
}

func assertAPIKeyNotExposed(t *testing.T, content, key string) {
	t.Helper()
	if strings.Contains(content, key) || strings.Contains(content, `"api_key"`) {
		t.Fatal("API credential leaked outside the encrypted credential store")
	}
}

func TestAPIKeySettingsEnableImmediatelyAndRemainPrivateAcrossRestart(t *testing.T) {
	var calls atomic.Int32
	keys := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveSettingsTestCatalog(w, r) {
			return
		}
		calls.Add(1)
		keys <- r.URL.Query().Get("api_key")
		if r.URL.Query().Get("action") != "getNumberV2" {
			t.Errorf("unexpected upstream action: %s", r.URL.Query().Get("action"))
		}
		_, _ = io.WriteString(w, `{"activationId":"test-activation","phoneNumber":"12025550123"}`)
	}))
	defer upstream.Close()
	dir := t.TempDir()
	var logs bytes.Buffer
	opts := liveSettingsOptions(dir, upstream.URL)
	opts.Logger = log.New(&logs, "", 0)
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if a != nil {
			_ = a.Close()
		}
	}()
	cookie := setupTestAdmin(t, a, testAdminPassword)
	if a.providerReady() {
		t.Fatal("unconfigured live provider was ready")
	}
	s := configuredResourceSettings()
	paddedKey := "  " + savedTestAPIKey + "\t"
	w := saveAPIKey(t, a, cookie, s, &paddedKey)
	assertAPIKeyNotExposed(t, w.Body.String(), savedTestAPIKey)
	if !a.providerReady() {
		t.Fatal("saving a key did not enable the provider immediately")
	}
	stored := encryptedAPIKey(t, a)
	decoded, decodeErr := base64.StdEncoding.DecodeString(stored)
	if decodeErr != nil {
		decoded, decodeErr = base64.RawStdEncoding.DecodeString(stored)
	}
	if decodeErr != nil || len(decoded) <= len(savedTestAPIKey) || strings.Contains(stored, savedTestAPIKey) || bytes.Contains(decoded, []byte(savedTestAPIKey)) {
		t.Fatal("saved API key was not encrypted at rest")
	}
	for _, key := range []*string{nil, new(string)} {
		s.Brand = "保存后的站点"
		saveAPIKey(t, a, cookie, s, key)
		if encryptedAPIKey(t, a) != stored || !a.providerReady() {
			t.Fatal("omitted or blank API key replaced the saved credential")
		}
	}
	for _, path := range []string{"/api/admin/settings", "/api/config", "/api/admin/overview", "/api/admin/cdks"} {
		w = apiRequest(a.Handler(), http.MethodGet, path, nil, "", cookie, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s HTTP %d", path, w.Code)
		}
		assertAPIKeyNotExposed(t, w.Body.String(), savedTestAPIKey)
		if path == "/api/admin/settings" {
			var settings struct {
				APIConfigured bool `json:"api_configured"`
			}
			if json.Unmarshal(w.Body.Bytes(), &settings) != nil || !settings.APIConfigured {
				t.Fatal("settings did not report the saved credential as configured")
			}
		}
	}
	code := issueSettingsTestCDK(t, a, cookie)
	for _, query := range []string{"SELECT value FROM settings", "SELECT snapshot FROM cdks", "SELECT action||object_id||detail FROM audit"} {
		rows, queryErr := a.db.Query(query)
		if queryErr != nil {
			t.Fatal(queryErr)
		}
		for rows.Next() {
			var content string
			if err = rows.Scan(&content); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			assertAPIKeyNotExposed(t, content, savedTestAPIKey)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
	}
	if calls.Load() != 0 {
		t.Fatal("saving settings or issuing CDKs purchased an upstream resource")
	}
	first := redeemSettingsPhone(t, a, code)
	if first.Order.Status != "waiting" || <-keys != savedTestAPIKey {
		t.Fatal("live allocation did not use the newly saved key without a restart")
	}
	assertAPIKeyNotExposed(t, logs.String(), savedTestAPIKey)
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a = nil
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.IsDir() || file.Name() == "instance.lock" {
			continue
		}
		content, readErr := os.ReadFile(filepath.Join(dir, file.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if bytes.Contains(content, []byte(savedTestAPIKey)) {
			t.Fatalf("plaintext API key found in persisted file %s", file.Name())
		}
	}
	// APIKey is the startup option populated from SMSBOWER_API_KEY by main.
	opts.APIKey = "different-environment-key-ignored-after-save"
	a, err = New(opts)
	if err != nil {
		t.Fatal(err)
	}
	cookie = adminCookie(t, a)
	code = issueSettingsTestCDK(t, a, cookie)
	second := redeemSettingsPhone(t, a, code)
	if second.Order.Status != "waiting" || <-keys != savedTestAPIKey || encryptedAPIKey(t, a) != stored {
		t.Fatal("restart did not prefer the persisted API credential over environment configuration")
	}
}

func TestAPIKeySettingsRejectInvalidValuesAtomically(t *testing.T) {
	opts := liveSettingsOptions(t.TempDir(), "http://127.0.0.1:1")
	opts.APIKey = savedTestAPIKey
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	cookie := setupTestAdmin(t, a, testAdminPassword)
	stored := encryptedAPIKey(t, a)
	original := a.currentSettings()
	for _, key := range []string{"short", strings.Repeat("k", 513), "embedded white-space", "embedded\nnewline", "embedded\x00null", "non-ascii-密钥", "embedded\x7fdelete"} {
		s := configuredResourceSettings()
		s.Brand = "不应保存"
		w := apiRequest(a.Handler(), http.MethodPut, "/api/admin/settings", apiKeySettings(s, &key), "", cookie, "http://example.test")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid key HTTP %d, want 400", w.Code)
		}
		if a.currentSettings() != original || encryptedAPIKey(t, a) != stored {
			t.Fatal("invalid key partially changed persisted settings")
		}
		assertAPIKeyNotExposed(t, w.Body.String(), savedTestAPIKey)
	}
}

func TestAPIKeySettingsRequireAdministratorAndSameOrigin(t *testing.T) {
	a, err := New(liveSettingsOptions(t.TempDir(), "http://127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	cookie := setupTestAdmin(t, a, testAdminPassword)
	key := savedTestAPIKey
	body := apiKeySettings(configuredResourceSettings(), &key)
	for _, tc := range []struct {
		cookie *http.Cookie
		origin string
		status int
	}{{nil, "http://example.test", http.StatusUnauthorized}, {cookie, "https://attacker.test", http.StatusForbidden}} {
		w := apiRequest(a.Handler(), http.MethodPut, "/api/admin/settings", body, "", tc.cookie, tc.origin)
		if w.Code != tc.status {
			t.Fatalf("unauthorized key change HTTP %d, want %d", w.Code, tc.status)
		}
	}
	var count int
	if err = a.db.QueryRow("SELECT COUNT(*) FROM metadata WHERE key='smsbower_api_key_v1'").Scan(&count); err != nil || count != 0 || a.providerReady() {
		t.Fatal("unauthorized key change mutated provider configuration")
	}
}

func TestAPIKeyRotationProtectsEveryUnsettledOrder(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveSettingsTestCatalog(w, r) {
			return
		}
		calls.Add(1)
		_, _ = io.WriteString(w, `{"activationId":"rotation-test","phoneNumber":"12025550123"}`)
	}))
	defer upstream.Close()
	opts := liveSettingsOptions(t.TempDir(), upstream.URL)
	opts.APIKey = savedTestAPIKey
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	cookie := setupTestAdmin(t, a, testAdminPassword)
	s := configuredResourceSettings()
	saveAPIKey(t, a, cookie, s, nil)
	order := redeemSettingsPhone(t, a, issueSettingsTestCDK(t, a, cookie))
	stored := encryptedAPIKey(t, a)
	newKey := "replacement-smsbower-api-key"
	for _, status := range []string{"allocating", "waiting", "received", "cancel_pending", "review", "next_pending", "next_uncertain", "complete_pending"} {
		if _, err = a.db.Exec("UPDATE orders SET status=? WHERE id=?", status, order.Order.ID); err != nil {
			t.Fatal(err)
		}
		s.Brand = "状态 " + status
		w := apiRequest(a.Handler(), http.MethodPut, "/api/admin/settings", apiKeySettings(s, &newKey), "", cookie, "http://example.test")
		if w.Code != http.StatusConflict || encryptedAPIKey(t, a) != stored || a.currentSettings().Brand == s.Brand {
			t.Fatalf("key rotation with %s order was not rejected atomically: HTTP %d", status, w.Code)
		}
		saveAPIKey(t, a, cookie, s, nil)
		sameKey := savedTestAPIKey
		saveAPIKey(t, a, cookie, s, &sameKey)
		if a.currentSettings() != s {
			t.Fatalf("ordinary settings change blocked by %s order", status)
		}
		stored = encryptedAPIKey(t, a)
	}
	if _, err = a.db.Exec("UPDATE orders SET status='completed' WHERE id=?", order.Order.ID); err != nil {
		t.Fatal(err)
	}
	saveAPIKey(t, a, cookie, s, &newKey)
	if encryptedAPIKey(t, a) == stored || calls.Load() != 1 {
		t.Fatal("settled-order rotation failed or caused an unexpected upstream request")
	}
}

func TestMissingAPIKeyCanBeRestoredWithExistingUnsettledOrder(t *testing.T) {
	a, err := New(liveSettingsOptions(t.TempDir(), "http://127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	cookie := setupTestAdmin(t, a, testAdminPassword)
	// Model a pre-existing live order whose credential has not yet been saved
	// through settings. Restoring access must remain possible for reconciliation.
	if err = a.seedDemo(); err != nil {
		t.Fatal(err)
	}
	_, err = a.db.Exec(`INSERT INTO orders(id,cdk_id,kind,status,provider_id,created_at,expires_at,cancel_after,attempt,max_attempts)
SELECT 'existing-order',id,'phone','waiting','existing-activation',0,0,0,1,2 FROM cdks WHERE kind='phone' LIMIT 1`)
	if err != nil {
		t.Fatal(err)
	}
	key := savedTestAPIKey
	saveAPIKey(t, a, cookie, configuredResourceSettings(), &key)
	if !a.providerReady() {
		t.Fatal("existing unsettled order prevented restoration of missing API credential")
	}
}

func TestAPIKeyWriteFailurePreservesSettingsAndLiveProvider(t *testing.T) {
	keys := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveSettingsTestCatalog(w, r) {
			return
		}
		keys <- r.URL.Query().Get("api_key")
		_, _ = io.WriteString(w, `{"activationId":"after-write-failure","phoneNumber":"12025550123"}`)
	}))
	defer upstream.Close()
	var logs bytes.Buffer
	opts := liveSettingsOptions(t.TempDir(), upstream.URL)
	opts.APIKey, opts.Logger = savedTestAPIKey, log.New(&logs, "", 0)
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	cookie := setupTestAdmin(t, a, testAdminPassword)
	original := configuredResourceSettings()
	saveAPIKey(t, a, cookie, original, nil)
	stored := encryptedAPIKey(t, a)
	_, err = a.db.Exec(`CREATE TRIGGER reject_credential_write BEFORE UPDATE ON metadata
WHEN NEW.key='smsbower_api_key_v1' BEGIN SELECT RAISE(ABORT, 'simulated credential write failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	changed := original
	changed.Brand = "不应部分保存"
	newKey := "replacement-key-that-must-not-take-effect"
	w := apiRequest(a.Handler(), http.MethodPut, "/api/admin/settings", apiKeySettings(changed, &newKey), "", cookie, "http://example.test")
	if w.Code != http.StatusInternalServerError || a.currentSettings() != original || encryptedAPIKey(t, a) != stored {
		t.Fatalf("credential write failure partially changed configuration: HTTP %d", w.Code)
	}
	var settingsJSON string
	var persisted Settings
	if err = a.db.QueryRow("SELECT value FROM settings WHERE id=1").Scan(&settingsJSON); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal([]byte(settingsJSON), &persisted) != nil || persisted != original {
		t.Fatal("settings transaction was not rolled back with failed credential storage")
	}
	assertAPIKeyNotExposed(t, w.Body.String()+logs.String(), savedTestAPIKey)
	assertAPIKeyNotExposed(t, w.Body.String()+logs.String(), newKey)
	order := redeemSettingsPhone(t, a, issueSettingsTestCDK(t, a, cookie))
	if order.Order.Status != "waiting" || <-keys != savedTestAPIKey {
		t.Fatal("failed credential write replaced or disabled the active provider")
	}
}

func TestAPIKeyTamperingFailsClosedWithoutEnvironmentFallback(t *testing.T) {
	for _, damage := range []string{"invalid-base64", "authentication-tag", "wrong-data-secret"} {
		t.Run(damage, func(t *testing.T) {
			dir := t.TempDir()
			opts := liveSettingsOptions(dir, "http://127.0.0.1:1")
			opts.APIKey = savedTestAPIKey
			a, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			stored := encryptedAPIKey(t, a)
			if err = a.Close(); err != nil {
				t.Fatal(err)
			}
			secretPath := filepath.Join(dir, "secret.key")
			originalSecret, err := os.ReadFile(secretPath)
			if err != nil {
				t.Fatal(err)
			}
			damaged := stored
			switch damage {
			case "invalid-base64":
				damaged = "not-valid-base64!"
			case "authentication-tag":
				ciphertext, decodeErr := base64.StdEncoding.DecodeString(stored)
				if decodeErr != nil {
					t.Fatal(decodeErr)
				}
				ciphertext[len(ciphertext)-1] ^= 1
				damaged = base64.StdEncoding.EncodeToString(ciphertext)
			case "wrong-data-secret":
				changedSecret := append([]byte(nil), originalSecret...)
				changedSecret[0] ^= 1
				if err = os.WriteFile(secretPath, changedSecret, 0600); err != nil {
					t.Fatal(err)
				}
			}
			db, err := openStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("UPDATE metadata SET value=? WHERE key='smsbower_api_key_v1'", damaged); err != nil {
				db.Close()
				t.Fatal(err)
			}
			db.Close()
			// A still-valid environment key must not silently select a different
			// account when the persisted credential cannot be authenticated.
			opts.APIKey = "fallback-key-that-must-not-be-used"
			a, err = New(opts)
			if err == nil || a != nil {
				if a != nil {
					a.Close()
				}
				t.Fatal("tampered credential did not prevent startup")
			}
			assertAPIKeyNotExposed(t, err.Error(), savedTestAPIKey)
			assertAPIKeyNotExposed(t, err.Error(), opts.APIKey)
			db, err = openStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			var after string
			if err = db.QueryRow("SELECT value FROM metadata WHERE key='smsbower_api_key_v1'").Scan(&after); err != nil || after != damaged {
				db.Close()
				t.Fatal("failed startup replaced the damaged credential")
			}
			_, err = db.Exec("UPDATE metadata SET value=? WHERE key='smsbower_api_key_v1'", stored)
			db.Close()
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(secretPath, originalSecret, 0600); err != nil {
				t.Fatal(err)
			}
			a, err = New(opts)
			if err != nil {
				t.Fatalf("failed startup left the database or process lock unusable: %v", err)
			}
			defer a.Close()
			if !a.providerReady() || encryptedAPIKey(t, a) != stored {
				t.Fatal("restoring the original encrypted credential did not recover the provider")
			}
		})
	}
}

func TestAPIKeyRotationWaitsForInFlightAllocation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveSettingsTestCatalog(w, r) {
			return
		}
		if r.URL.Query().Get("api_key") != savedTestAPIKey {
			t.Error("in-flight allocation used an unexpected key")
		}
		close(entered)
		<-release
		_, _ = io.WriteString(w, `{"activationId":"in-flight-test","phoneNumber":"12025550123"}`)
	}))
	defer upstream.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	opts := liveSettingsOptions(t.TempDir(), upstream.URL)
	opts.APIKey = savedTestAPIKey
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	cookie := setupTestAdmin(t, a, testAdminPassword)
	s := configuredResourceSettings()
	saveAPIKey(t, a, cookie, s, nil)
	code := issueSettingsTestCDK(t, a, cookie)
	allocation := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		allocation <- apiRequest(a.Handler(), http.MethodPost, "/api/redeem", map[string]string{"cdk": code, "phone_country": "0", "phone_provider_id": "2368"}, "", nil, "")
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("allocation did not reach the local upstream")
	}
	rotation := make(chan *httptest.ResponseRecorder, 1)
	newKey := "concurrent-replacement-api-key"
	go func() {
		rotation <- apiRequest(a.Handler(), http.MethodPut, "/api/admin/settings", apiKeySettings(s, &newKey), "", cookie, "http://example.test")
	}()
	select {
	case w := <-rotation:
		t.Fatalf("rotation completed during an in-flight allocation: HTTP %d", w.Code)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case w := <-allocation:
		if parseOrder(t, w).Order.Status != "waiting" {
			t.Fatal("allocation failed while the key change was waiting")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("allocation and key change deadlocked")
	}
	select {
	case w := <-rotation:
		if w.Code != http.StatusConflict {
			t.Fatalf("rotation abandoned an allocated resource: HTTP %d", w.Code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("key change did not resume after allocation")
	}
}
