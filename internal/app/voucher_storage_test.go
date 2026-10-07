package app

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Build the complete pre-quota schema rather than deriving a fixture from the
// current schema, so column additions and old-record preservation are exercised.
func legacyVoucherStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "atelier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE settings (id INTEGER PRIMARY KEY CHECK(id=1), value TEXT NOT NULL);
CREATE TABLE metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE admin_credentials (id INTEGER PRIMARY KEY CHECK(id=1), salt BLOB NOT NULL CHECK(length(salt)=16), hash BLOB NOT NULL CHECK(length(hash)=32), created_at INTEGER NOT NULL);
CREATE TABLE cdks (id TEXT PRIMARY KEY, hash TEXT NOT NULL UNIQUE, masked_code TEXT NOT NULL, batch_id TEXT NOT NULL, kind TEXT NOT NULL, status TEXT NOT NULL, note TEXT NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, max_attempts INTEGER NOT NULL, snapshot TEXT NOT NULL);
CREATE TABLE orders (id TEXT PRIMARY KEY, cdk_id TEXT NOT NULL REFERENCES cdks(id), kind TEXT NOT NULL, status TEXT NOT NULL, provider_id TEXT NOT NULL DEFAULT '', resource TEXT NOT NULL DEFAULT '', code TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, cancel_after INTEGER NOT NULL, attempt INTEGER NOT NULL, max_attempts INTEGER NOT NULL, message TEXT NOT NULL DEFAULT '', cancel_reason TEXT NOT NULL DEFAULT '', last_poll INTEGER NOT NULL DEFAULT 0);
CREATE INDEX order_cdk_idx ON orders(cdk_id,created_at DESC);
CREATE INDEX order_status_idx ON orders(status);
CREATE INDEX cdk_created_idx ON cdks(created_at DESC);
CREATE TABLE audit (id INTEGER PRIMARY KEY, created_at INTEGER NOT NULL, action TEXT NOT NULL, object_id TEXT NOT NULL, detail TEXT NOT NULL);`)
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := json.Marshal(defaultSettings())
	now := time.Now().UTC().Add(-time.Minute)
	for _, c := range []struct {
		id, status string
		limit      int
	}{{"legacy", "used", 3}, {"received", "used", 1}, {"manual", "used", 2}, {"manual-no-audit", "used", 1}, {"disabled", "disabled", 3}, {"unsettled", "active", 3}} {
		_, err = db.Exec(`INSERT INTO cdks(id,hash,masked_code,batch_id,kind,status,note,created_at,expires_at,attempts,max_attempts,snapshot) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, c.id, hash(c.id), c.id+"-••••", "old-batch", "email", c.status, "保留备注", millis(now), millis(now.Add(time.Hour)), 5, c.limit, string(settings))
		if err != nil {
			t.Fatal(err)
		}
	}
	for i, o := range []struct{ id, cdk, status, code string }{
		{"old-received", "legacy", "completed", "123456"},
		{"latest-cancelled", "legacy", "cancelled", ""},
		{"received-order", "received", "received", "678901"},
		{"manual-order", "manual", "completed", ""},
		{"manual-no-audit-order", "manual-no-audit", "completed", ""},
		{"disabled-order", "disabled", "completed", "654321"},
		{"uncertain-order", "unsettled", "allocating", ""},
	} {
		_, err = db.Exec(`INSERT INTO orders(id,cdk_id,kind,status,provider_id,resource,code,created_at,expires_at,cancel_after,attempt,max_attempts,last_poll) VALUES(?,?,'email',?,?,?,?,?,?,?,?,?,?)`, o.id, o.cdk, o.status, "upstream-"+o.id, o.id+"@example.test", o.code, millis(now.Add(time.Duration(i)*time.Second)), millis(now.Add(time.Hour)), millis(now), i+1, 3, millis(now.Add(10*time.Second)))
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = db.Exec("INSERT INTO audit(created_at,action,object_id,detail) VALUES(?,?,?,?)", millis(now), "resolve_order", "manual-order", "consumed")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func storageApp(t *testing.T, dir string) *App {
	t.Helper()
	a, err := New(Options{DataDir: dir, Mode: "live", DisableWorker: true, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestVoucherMigrationPreservesLegacyDataAndSuccessQuota(t *testing.T) {
	dir := legacyVoucherStore(t)
	a := storageApp(t, dir)
	for _, expected := range []struct {
		id, status  string
		limit, used int
	}{{"legacy", "available", 3, 1}, {"received", "active", 1, 1}, {"manual", "available", 2, 1}, {"manual-no-audit", "used", 1, 1}, {"disabled", "disabled", 3, 1}, {"unsettled", "review", 3, 0}} {
		c, err := a.getCDK(expected.id)
		if err != nil {
			t.Fatal(err)
		}
		if c.Status != expected.status || c.UsageLimit != expected.limit || c.UsedCount != expected.used {
			t.Errorf("%s status/quota = %s %d/%d, want %s %d/%d", c.ID, c.Status, c.UsedCount, c.UsageLimit, expected.status, expected.used, expected.limit)
		}
		if c.CodeCipher != "" || c.Code != "" || c.Attempts != 5 || c.Note != "保留备注" || c.BatchID != "old-batch" {
			t.Errorf("legacy data was changed or invented: %+v", c)
		}
	}
	o, err := a.getOrder("old-received")
	if err != nil {
		t.Fatal(err)
	}
	if !o.UsageCounted || o.MailRound != 1 || len(o.Codes) != 1 || o.Codes[0].Code != o.Code || o.Codes[0].Round != 1 || o.Codes[0].ReceivedAt.IsZero() {
		t.Fatalf("missing migrated receipt: %+v", o)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a = storageApp(t, dir)
	defer a.Close()
	c, _ := a.getCDK("legacy")
	if c.UsedCount != 1 || c.UsageLimit != 3 || c.Status != "available" {
		t.Fatalf("restart repeated or changed migration: %+v", c)
	}
}

func TestEncryptedVoucherHydrationAndLegacyBackfill(t *testing.T) {
	a := storageApp(t, legacyVoucherStore(t))
	defer a.Close()
	c, _ := a.getCDK("legacy")
	if err := a.hydrateCDKCode(&c); err != nil || c.Code != "" {
		t.Fatal("a legacy hash-only code must remain unknown")
	}
	if err := a.rememberCDKCode(c.ID, "wrong-code"); err != nil {
		t.Fatal(err)
	}
	c, _ = a.getCDK(c.ID)
	if c.CodeCipher != "" {
		t.Fatal("a wrong voucher was saved")
	}
	// Existing hash was deliberately generated from lower-case input for this
	// fixture; replace it with a real normalized voucher before backfilling.
	code := "EM-ABCDEF-234567-GHIJKL-MNOPQR"
	if _, err := a.db.Exec("UPDATE cdks SET hash=? WHERE id=?", hash(code), c.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.rememberCDKCode(c.ID, " "+strings.ToLower(code)+" "); err != nil {
		t.Fatal(err)
	}
	c, _ = a.getCDK(c.ID)
	if c.Code != "" || c.CodeCipher == "" || strings.Contains(c.CodeCipher, code) {
		t.Fatal("stored or ordinary fetch exposed a plaintext voucher")
	}
	encrypted := c.CodeCipher
	if err := a.hydrateCDKCode(&c); err != nil || c.Code != code {
		t.Fatalf("authorized hydration failed: %v", err)
	}
	payload, _ := json.Marshal(c)
	if bytes.Contains(payload, []byte(encrypted)) || bytes.Contains(payload, []byte(c.Hash)) {
		t.Fatal("admin response leaked encrypted storage or hash")
	}
	if err := a.rememberCDKCode(c.ID, code); err != nil {
		t.Fatal(err)
	}
	c, _ = a.getCDK(c.ID)
	if c.CodeCipher != encrypted {
		t.Fatal("legacy backfill overwrote an already-saved code")
	}
	other, err := a.encryptCDKCode(code)
	if err != nil || other == encrypted {
		t.Fatal("encryption did not use a fresh nonce")
	}
	c.CodeCipher = encrypted[:len(encrypted)/2]
	if err := a.hydrateCDKCode(&c); err == nil || c.Code != "" {
		t.Fatal("tampered ciphertext was accepted")
	}
	c.CodeCipher = encrypted
	c.Hash = hash("different")
	if err := a.hydrateCDKCode(&c); err == nil {
		t.Fatal("ciphertext copied from a different voucher was accepted")
	}
	if _, err := a.decryptAPIKey(encrypted); err == nil {
		t.Fatal("CDK ciphertext was accepted in the API-key encryption domain")
	}
}

func TestVoucherUsageCountedExactlyOnceAcrossMailRounds(t *testing.T) {
	a := storageApp(t, legacyVoucherStore(t))
	defer a.Close()
	o, _ := a.getOrder("latest-cancelled")
	o.Status, o.Code, o.UsageCounted = "received", "112233", true
	o.Codes = []ReceivedCode{{Round: 1, Code: o.Code, ReceivedAt: time.Now().UTC()}}
	if err := a.saveOrder(&o, "active"); err != nil {
		t.Fatal(err)
	}
	if err := a.saveOrder(&o, "active"); err != nil {
		t.Fatal(err)
	}
	c, _ := a.getCDK(o.CDKID)
	if c.UsedCount != 2 {
		t.Fatalf("duplicate first-code save consumed quota twice: %d", c.UsedCount)
	}
	o.MailRound, o.Status, o.Code, o.UsageCounted = 2, "waiting", "", false
	if err := a.saveOrder(&o, "active"); err != nil {
		t.Fatal(err)
	}
	o, _ = a.getOrder(o.ID)
	if !o.UsageCounted || o.Code != "" || len(o.Codes) != 1 || o.MailRound != 2 {
		t.Fatalf("next round discarded receipt/accounting: %+v", o)
	}
	for round := 2; round <= 3; round++ {
		o.MailRound, o.Status, o.Code = round, "received", "112233"
		o.Codes = append(o.Codes, ReceivedCode{Round: round, Code: o.Code, ReceivedAt: time.Now().UTC()})
		if err := a.saveOrder(&o, "active"); err != nil {
			t.Fatal(err)
		}
	}
	c, _ = a.getCDK(o.CDKID)
	if c.UsedCount != 2 {
		t.Fatalf("repeat email rounds consumed another resource quota: %d", c.UsedCount)
	}
	o, _ = a.getOrder(o.ID)
	if len(o.Codes) != 3 || o.MailRound != 3 {
		t.Fatalf("mail history not preserved: %+v", o)
	}
	a.decorate(&o)
	if o.CanNextCode || o.UsedCount != 2 || o.UsageLimit != 3 {
		t.Fatalf("wrong decorated quota or fourth-round permission: %+v", o)
	}
}

func TestVoucherRecoveryUsesLatestOrderAndPreservesUncertainNextRound(t *testing.T) {
	a := storageApp(t, legacyVoucherStore(t))
	defer a.Close()
	o, _ := a.getOrder("received-order")
	a.decorate(&o)
	if !o.CanNextCode || o.CanRetry || o.UsedCount != 1 || o.UsageLimit != 1 {
		t.Fatalf("fully used voucher lost its acquired mailbox continuation: %+v", o)
	}
	o.Status, o.MailRound, o.Code = "next_pending", 2, ""
	if err := a.saveOrder(&o, "active"); err != nil {
		t.Fatal(err)
	}
	if err := a.recoverOrders(); err != nil {
		t.Fatal(err)
	}
	o, _ = a.getOrder(o.ID)
	c, _ := a.getCDK(o.CDKID)
	if o.Status != "next_uncertain" || c.Status != "review" || len(o.Codes) != 1 || !o.UsageCounted || c.UsedCount != 1 {
		t.Fatalf("uncertain next-round restart was not preserved: order=%+v voucher=%+v", o, c)
	}
	legacy, _ := a.getCDK("legacy")
	if legacy.Status != "available" {
		t.Fatal("an old received order masked the latest cancelled order")
	}
}

func TestVoucherUsageSaveRollsBackCounterWithOrderFailure(t *testing.T) {
	a := storageApp(t, legacyVoucherStore(t))
	defer a.Close()
	o, _ := a.getOrder("latest-cancelled")
	o.UsageCounted, o.Status = true, "received"
	if _, err := a.db.Exec("CREATE TRIGGER refuse_voucher_update BEFORE UPDATE ON cdks BEGIN SELECT RAISE(ABORT,'fixture failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := a.saveOrder(&o, "active"); err == nil {
		t.Fatal("save unexpectedly succeeded")
	}
	saved, _ := a.getOrder(o.ID)
	c, _ := a.getCDK(o.CDKID)
	if saved.UsageCounted || saved.Status != "cancelled" || c.UsedCount != 1 {
		t.Fatal("failed transaction committed partial usage or order state")
	}
}

func TestVoucherReleaseUsesRemainingQuotaWithinReceiptTransaction(t *testing.T) {
	a := storageApp(t, legacyVoucherStore(t))
	defer a.Close()
	o, _ := a.getOrder("manual-order")
	if _, err := a.db.Exec("UPDATE cdks SET usage_limit=1 WHERE id=?", o.CDKID); err != nil {
		t.Fatal(err)
	}
	o.Status = "completed"
	if err := a.saveOrder(&o, "available"); err != nil {
		t.Fatal(err)
	}
	c, _ := a.getCDK(o.CDKID)
	if c.Status != "used" || c.UsedCount != 1 {
		t.Fatalf("exhausted voucher was released: %+v", c)
	}
	o, _ = a.getOrder("latest-cancelled")
	if _, err := a.db.Exec("UPDATE cdks SET usage_limit=2 WHERE id=?", o.CDKID); err != nil {
		t.Fatal(err)
	}
	o.Status, o.UsageCounted = "completed", true
	if err := a.saveOrder(&o, "available"); err != nil {
		t.Fatal(err)
	}
	c, _ = a.getCDK(o.CDKID)
	if c.Status != "used" || c.UsedCount != 2 {
		t.Fatalf("first receipt and final quota release disagreed: %+v", c)
	}
}

func TestVoucherThirdMailMayReuseFirstRoundCode(t *testing.T) {
	f := newQuotaHarness(t, 1)
	session := redeem(t, f.app, f.code)
	for round, code := range []string{"123456", "654321", "123456"} {
		if round > 0 {
			session = quotaNext(t, f.app, session, round)
		}
		f.p.setCode(code)
		session = quotaPoll(t, f.app, session)
		if session.Order.Status != "received" || len(session.Order.Codes) != round+1 {
			t.Fatalf("round %d was discarded because its code matches an older nonadjacent round: %+v", round+1, session.Order)
		}
	}
	quotaAssertUsage(t, f.app, session, 1, 1)
}

func TestVoucherNextAcknowledgmentSaveFailureRecoversWithoutRestart(t *testing.T) {
	f := newQuotaHarness(t, 2)
	session := redeem(t, f.app, f.code)
	f.p.setCode("123456")
	session = quotaPoll(t, f.app, session)
	_, err := f.app.db.Exec(`CREATE TRIGGER reject_next_ack BEFORE UPDATE ON orders
WHEN OLD.status='next_pending' AND NEW.status='waiting'
BEGIN SELECT RAISE(ABORT,'test acknowledgment save failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	response := apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/next-code", map[string]int{"round": 1}, session.Token, nil, "")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("acknowledgment persistence fault HTTP %d", response.Code)
	}
	if _, err = f.app.db.Exec("DROP TRIGGER reject_next_ack"); err != nil {
		t.Fatal(err)
	}
	session = quotaPoll(t, f.app, session)
	if session.Order.Status == "next_pending" {
		t.Fatal("a temporary save failure left continuation permanently pending until restart")
	}
	if f.p.nextCount() != 1 || len(session.Order.Codes) != 1 {
		t.Fatal("reconciliation repeated the continuation request or lost the first code")
	}
	f.p.setCode("654321")
	session = quotaPoll(t, f.app, session)
	if session.Order.Status != "received" || len(session.Order.Codes) != 2 || f.p.nextCount() != 1 {
		t.Fatalf("continuation did not reconcile after storage recovered: %+v", session.Order)
	}
	quotaAssertUsage(t, f.app, session, 1, 2)
}
