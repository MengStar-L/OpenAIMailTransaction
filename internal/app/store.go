package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"time"
)

func openStore(dir string) (*sql.DB, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "atelier.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS settings (id INTEGER PRIMARY KEY CHECK(id=1), value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS admin_credentials (
 id INTEGER PRIMARY KEY CHECK(id=1), salt BLOB NOT NULL CHECK(length(salt)=16),
 hash BLOB NOT NULL CHECK(length(hash)=32), created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS cdks (
 id TEXT PRIMARY KEY, hash TEXT NOT NULL UNIQUE, masked_code TEXT NOT NULL, batch_id TEXT NOT NULL,
 kind TEXT NOT NULL, status TEXT NOT NULL, note TEXT NOT NULL, created_at INTEGER NOT NULL,
 expires_at INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, max_attempts INTEGER NOT NULL,
 snapshot TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS orders (
 id TEXT PRIMARY KEY, cdk_id TEXT REFERENCES cdks(id), kind TEXT NOT NULL, status TEXT NOT NULL,
 provider_id TEXT NOT NULL DEFAULT '', resource TEXT NOT NULL DEFAULT '', code TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, cancel_after INTEGER NOT NULL,
 attempt INTEGER NOT NULL, max_attempts INTEGER NOT NULL, message TEXT NOT NULL DEFAULT '',
 cancel_reason TEXT NOT NULL DEFAULT '', last_poll INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS order_cdk_idx ON orders(cdk_id,created_at DESC);
CREATE INDEX IF NOT EXISTS order_status_idx ON orders(status);
CREATE INDEX IF NOT EXISTS cdk_created_idx ON cdks(created_at DESC);
CREATE TABLE IF NOT EXISTS audit (id INTEGER PRIMARY KEY, created_at INTEGER NOT NULL, action TEXT NOT NULL, object_id TEXT NOT NULL, detail TEXT NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = migrateVoucherUsage(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate voucher usage: %w", err)
	}
	if err = migrateAdminResources(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate administrator resources: %w", err)
	}
	if err = migrateAllocationQueue(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate allocation queue: %w", err)
	}
	if err = migrateCompletionState(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate completion state: %w", err)
	}
	if err = migrateAutomaticMail(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate automatic mail: %w", err)
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS order_phone_channels (order_id TEXT PRIMARY KEY REFERENCES orders(id), channel_json TEXT NOT NULL);`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS phone_unavailable (
 credential TEXT NOT NULL, service TEXT NOT NULL, country TEXT NOT NULL,
 provider_id TEXT NOT NULL, expires_at INTEGER NOT NULL,
 PRIMARY KEY(credential,service,country,provider_id));`); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Schema changes and data backfills commit together so an interrupted upgrade
// never leaves a partially migrated quota or receipt history.
func migrateVoucherUsage(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	columns := map[string]map[string]bool{}
	for _, table := range []string{"cdks", "orders"} {
		rows, err := tx.Query("PRAGMA table_info(" + table + ")")
		if err != nil {
			return err
		}
		columns[table] = map[string]bool{}
		for rows.Next() {
			var ordinal, required, primary int
			var name, kind string
			var defaultValue any
			if err = rows.Scan(&ordinal, &name, &kind, &required, &defaultValue, &primary); err != nil {
				rows.Close()
				return err
			}
			columns[table][name] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	additions := []struct{ table, column, definition string }{
		{"cdks", "code_cipher", "TEXT NOT NULL DEFAULT ''"},
		{"cdks", "usage_limit", "INTEGER NOT NULL DEFAULT 1"},
		{"cdks", "used_count", "INTEGER NOT NULL DEFAULT 0"},
		{"orders", "mail_round", "INTEGER NOT NULL DEFAULT 1"},
		{"orders", "codes", "TEXT NOT NULL DEFAULT '[]'"},
		{"orders", "usage_counted", "INTEGER NOT NULL DEFAULT 0"},
		{"orders", "round_wait_seen", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, addition := range additions {
		if !columns[addition.table][addition.column] {
			if _, err = tx.Exec("ALTER TABLE " + addition.table + " ADD COLUMN " + addition.column + " " + addition.definition); err != nil {
				return err
			}
		}
	}
	if !columns["cdks"]["usage_limit"] {
		if _, err = tx.Exec("UPDATE cdks SET usage_limit=MAX(1,max_attempts)"); err != nil {
			return err
		}
	}
	if !columns["orders"]["codes"] {
		rows, err := tx.Query("SELECT id,code,created_at,last_poll FROM orders WHERE code<>''")
		if err != nil {
			return err
		}
		type oldReceipt struct{ id, codes string }
		var receipts []oldReceipt
		for rows.Next() {
			var id, code string
			var created, lastPoll int64
			if err = rows.Scan(&id, &code, &created, &lastPoll); err != nil {
				rows.Close()
				return err
			}
			if lastPoll == 0 {
				lastPoll = created
			}
			history, err := json.Marshal([]ReceivedCode{{Round: 1, Code: code, ReceivedAt: stamp(lastPoll)}})
			if err != nil {
				rows.Close()
				return err
			}
			receipts = append(receipts, oldReceipt{id, string(history)})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, receipt := range receipts {
			if _, err = tx.Exec("UPDATE orders SET codes=? WHERE id=?", receipt.codes, receipt.id); err != nil {
				return err
			}
		}
	}
	if !columns["orders"]["usage_counted"] {
		// Before quota support, a code-less completed order could only be an
		// administrator's confirmed consumption. Audit writes are best-effort,
		// so preserve that consumption even when its audit entry is unavailable.
		if _, err = tx.Exec(`UPDATE orders SET usage_counted=1 WHERE code<>'' OR status='completed' OR json_array_length(codes)>0 OR
EXISTS (SELECT 1 FROM audit WHERE action='resolve_order' AND object_id=orders.id AND detail='consumed')`); err != nil {
			return err
		}
	}
	if !columns["cdks"]["used_count"] {
		if _, err = tx.Exec("UPDATE cdks SET used_count=(SELECT COUNT(*) FROM orders WHERE cdk_id=cdks.id AND usage_counted=1)"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const cdkColumns = `id,hash,masked_code,batch_id,kind,status,note,created_at,expires_at,attempts,max_attempts,snapshot,code_cipher,usage_limit,used_count`
const orderColumns = `id,cdk_id,kind,status,provider_id,resource,code,created_at,expires_at,cancel_after,attempt,max_attempts,message,cancel_reason,last_poll,mail_round,codes,usage_counted,round_wait_seen,admin_request_id`

type scanner interface{ Scan(...any) error }

func scanCDK(row scanner) (c CDK, err error) {
	var created, expires int64
	var snap string
	err = row.Scan(&c.ID, &c.Hash, &c.MaskedCode, &c.BatchID, &c.Kind, &c.Status, &c.Note, &created, &expires, &c.Attempts, &c.MaxAttempts, &snap, &c.CodeCipher, &c.UsageLimit, &c.UsedCount)
	if err != nil {
		return
	}
	c.CreatedAt = stamp(created)
	c.ExpiresAt = stamp(expires)
	err = json.Unmarshal([]byte(snap), &c.Snapshot)
	return
}
func scanOrder(row scanner) (o Order, err error) {
	var created, expires, cancel, last int64
	var codes string
	var cdkID sql.NullString
	err = row.Scan(&o.ID, &cdkID, &o.Kind, &o.Status, &o.ProviderID, &o.Resource, &o.Code, &created, &expires, &cancel, &o.Attempt, &o.MaxAttempts, &o.Message, &o.CancelReason, &last, &o.MailRound, &codes, &o.UsageCounted, &o.RoundWaitSeen, &o.RequestID)
	if err != nil {
		return
	}
	o.CDKID = cdkID.String
	o.Source = "cdk"
	if !cdkID.Valid {
		o.Source = "admin"
	}
	o.CreatedAt = stamp(created)
	o.ExpiresAt = stamp(expires)
	o.CancelAfter = stamp(cancel)
	o.LastPoll = stamp(last)
	err = json.Unmarshal([]byte(codes), &o.Codes)
	if o.Codes == nil {
		o.Codes = []ReceivedCode{}
	}
	return
}
func stamp(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.UnixMilli(n).UTC()
}
func millis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
func (a *App) getCDK(id string) (CDK, error) {
	return scanCDK(a.db.QueryRow("SELECT "+cdkColumns+" FROM cdks WHERE id=?", id))
}
func (a *App) getOrder(id string) (Order, error) {
	return scanOrder(a.db.QueryRow("SELECT "+orderColumns+" FROM orders WHERE id=?", id))
}
func (a *App) latestOrder(cdkID string) (Order, error) {
	return scanOrder(a.db.QueryRow("SELECT "+orderColumns+" FROM orders WHERE cdk_id=? ORDER BY created_at DESC,rowid DESC LIMIT 1", cdkID))
}

// Order and voucher transitions are committed atomically. A voucher can never be
// released without the corresponding confirmed terminal order being persisted.
func (a *App) saveOrder(o *Order, cdkStatus string) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var counted bool
	if err = tx.QueryRow("SELECT usage_counted FROM orders WHERE id=? AND cdk_id IS ?", o.ID, nullableCDKID(o.CDKID)).Scan(&counted); err != nil {
		return err
	}
	// Receipt use cannot be undone by a later poll, cancellation or stale write.
	o.UsageCounted = counted || o.UsageCounted
	if o.Codes == nil {
		o.Codes = []ReceivedCode{}
	}
	if o.MailRound < 1 {
		o.MailRound = 1
	}
	codes, err := json.Marshal(o.Codes)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE orders SET status=?,provider_id=?,resource=?,code=?,expires_at=?,cancel_after=?,message=?,cancel_reason=?,last_poll=?,mail_round=?,codes=?,usage_counted=?,round_wait_seen=? WHERE id=?`, o.Status, o.ProviderID, o.Resource, o.Code, millis(o.ExpiresAt), millis(o.CancelAfter), o.Message, o.CancelReason, millis(o.LastPoll), o.MailRound, string(codes), o.UsageCounted, o.RoundWaitSeen, o.ID)
	if err != nil {
		return err
	}
	if o.CDKID != "" && !counted && o.UsageCounted {
		if _, err = tx.Exec("UPDATE cdks SET used_count=used_count+1 WHERE id=?", o.CDKID); err != nil {
			return err
		}
	}
	if o.CDKID != "" && cdkStatus != "" {
		if _, err = tx.Exec("UPDATE cdks SET status=CASE WHEN status='disabled' THEN 'disabled' WHEN ?='available' AND used_count>=usage_limit THEN 'used' ELSE ? END WHERE id=?", cdkStatus, cdkStatus, o.CDKID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (a *App) audit(action, id, detail string) {
	if _, err := a.db.Exec("INSERT INTO audit(created_at,action,object_id,detail) VALUES(?,?,?,?)", time.Now().UnixMilli(), action, id, detail); err != nil {
		a.log.Printf("audit persistence failed: %v", err)
	}
}
func (a *App) decorate(o *Order) {
	a.decoratePhoneChannel(o)
	a.decorateQueue(o)
	defer a.decorateAutoNext(o)
	if o.CDKID == "" {
		o.Source = "admin"
		o.UsageLimit, o.UsedCount, o.Attempt, o.MaxAttempts = 0, 0, 0, 0
		o.CanRetry = terminalOrder(o.Status)
		o.CanNextCode = o.Kind == "email" && o.Status == "received" && o.MailRound < 3 && time.Now().Before(o.ExpiresAt)
		return
	}
	o.Source = "cdk"
	c, err := a.getCDK(o.CDKID)
	if err == nil {
		o.UsageLimit = c.UsageLimit
		o.UsedCount = c.UsedCount
		latest, latestErr := a.latestOrder(c.ID)
		o.CanRetry = latestErr == nil && latest.ID == o.ID && c.Status == "available" && c.UsedCount < c.UsageLimit && time.Now().Before(c.ExpiresAt)
		o.CanNextCode = c.Status != "disabled" && o.Kind == "email" && o.Status == "received" && o.MailRound < 3 && time.Now().Before(o.ExpiresAt)
	}
}
func (a *App) recoverOrders() error {
	tx, err := a.db.Begin()
	if err != nil {
		return fmt.Errorf("recover orders: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.Exec(`UPDATE orders SET status='review', message='购买结果待核对，请联系管理员' WHERE status='allocating';
UPDATE orders SET status='next_uncertain', message='续收结果待核对，请联系管理员' WHERE status='next_pending';
UPDATE cdks SET status=CASE
 WHEN (SELECT status FROM orders WHERE cdk_id=cdks.id ORDER BY created_at DESC,rowid DESC LIMIT 1) IN ('review','next_uncertain') THEN 'review'
 WHEN (SELECT status FROM orders WHERE cdk_id=cdks.id ORDER BY created_at DESC,rowid DESC LIMIT 1) IN ('queued','waiting','received','cancel_pending','complete_pending') THEN 'active'
 WHEN used_count>=usage_limit THEN 'used' ELSE 'available' END
WHERE status<>'disabled' AND EXISTS(SELECT 1 FROM orders WHERE cdk_id=cdks.id);`)
	if err != nil {
		return fmt.Errorf("recover orders: %w", err)
	}
	return tx.Commit()
}
