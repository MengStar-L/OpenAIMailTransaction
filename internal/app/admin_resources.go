package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"time"

	"openai-mail-transaction/internal/provider"
)

// A missing voucher is an administrator purchase, never an unlimited voucher.
func nullableCDKID(id string) any {
	if id == "" {
		return nil
	}
	return id
}

func orderLockKey(o Order) string {
	if o.CDKID == "" {
		return "admin-resource:" + o.Kind
	}
	return o.CDKID
}

type adminResourceContextKey struct{}

// Only the cookie-authenticated route wrapper can set this context value.
// Public order endpoints continue to require a voucher bearer session.
func (a *App) adminResourceAction(next http.HandlerFunc) http.HandlerFunc {
	return a.admin(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), adminResourceContextKey{}, r.PathValue("id"))
		next(w, r.WithContext(ctx))
	})
}

func (a *App) actionOrder(r *http.Request) (Order, error) {
	if id, ok := r.Context().Value(adminResourceContextKey{}).(string); ok {
		o, err := a.getOrder(id)
		if err != nil || o.CDKID != "" {
			return Order{}, errors.New("管理员资源不存在")
		}
		return o, nil
	}
	return a.tokenOrder(r)
}

var resourceRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

func (a *App) listAdminResources(w http.ResponseWriter, r *http.Request) {
	orders := []Order{}
	for _, kind := range []string{"email", "phone"} {
		o, err := a.latestAdminResource(kind)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			a.databaseError(w, err)
			return
		}
		a.decorate(&o)
		orders = append(orders, o)
	}
	respond(w, 200, map[string]any{"orders": orders, "server_time": time.Now().UTC()})
}

func (a *App) latestAdminResource(kind string) (Order, error) {
	return scanOrder(a.db.QueryRow("SELECT "+orderColumns+" FROM orders WHERE cdk_id IS NULL AND kind=? ORDER BY created_at DESC,rowid DESC LIMIT 1", kind))
}

func (a *App) allocateAdminResource(w http.ResponseWriter, r *http.Request) {
	var in struct {
		phoneSelection
		Kind         string `json:"kind"`
		RequestID    string `json:"request_id"`
		QueueMinutes *int   `json:"queue_minutes"`
	}
	if !decode(w, r, &in) {
		return
	}
	queueMinutes, err := requestedQueueMinutes(in.QueueMinutes)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if (in.Kind != "phone" && in.Kind != "email") || !resourceRequestIDPattern.MatchString(in.RequestID) {
		fail(w, 400, "资源类型或请求编号无效")
		return
	}
	a.providerGate.RLock()
	defer a.providerGate.RUnlock()
	if a.updateMaintenance.Load() {
		fail(w, http.StatusServiceUnavailable, "正在更新程序，请稍后重试")
		return
	}
	requestUnlock := a.lock("admin-request:" + in.RequestID)
	defer requestUnlock()
	unlock := a.lock("admin-resource:" + in.Kind)
	defer unlock()
	var existingID, existingKind string
	err = a.db.QueryRow("SELECT order_id,kind FROM admin_resource_requests WHERE request_id=?", in.RequestID).Scan(&existingID, &existingKind)
	if err == nil {
		if existingKind != in.Kind {
			fail(w, 409, "该请求编号已用于其他资源")
			return
		}
		o, e := a.getOrder(existingID)
		if e != nil {
			a.databaseError(w, e)
			return
		}
		a.orderResponse(w, o, false)
		return
	}
	if err != sql.ErrNoRows {
		a.databaseError(w, err)
		return
	}
	previous, err := a.latestAdminResource(in.Kind)
	if err != nil && err != sql.ErrNoRows {
		a.databaseError(w, err)
		return
	}
	if err == nil && !terminalOrder(previous.Status) {
		// Record every accepted retry key, even when it resumes an earlier
		// purchase. A delayed retry must never allocate after that order ends.
		if _, err = a.db.Exec("INSERT INTO admin_resource_requests(request_id,kind,order_id) VALUES(?,?,?)", in.RequestID, in.Kind, previous.ID); err != nil {
			a.databaseError(w, err)
			return
		}
		a.orderResponse(w, previous, false)
		return
	}
	s := a.currentSettings()
	if err = a.validateAdminResource(in.Kind, s); err != nil {
		adminAllocationRejected(w, err)
		return
	}
	ttl := time.Duration(s.PhoneTTLMinutes) * time.Minute
	request := provider.Request{Kind: in.Kind, Service: s.PhoneService, Country: s.PhoneCountry, MaxPrice: s.PhoneMaxPrice}
	var selectedChannel *provider.PhoneChannel
	if in.Kind == "phone" {
		request, selectedChannel, err = a.selectedPhoneRequest(s, in.phoneSelection)
		if err != nil {
			adminAllocationRejected(w, err)
			return
		}
	}
	if in.Kind == "email" {
		ttl = time.Duration(s.EmailTTLMinutes) * time.Minute
		request.Service, request.Domain, request.Country, request.MaxPrice = s.EmailService, s.EmailDomain, "", s.EmailMaxPrice
	}
	request.TTL = ttl
	now := time.Now().UTC()
	o := Order{ID: randomString(12), Source: "admin", RequestID: in.RequestID, Kind: in.Kind, Status: "allocating", CreatedAt: now, ExpiresAt: now.Add(ttl), CancelAfter: now, MailRound: 1, Codes: []ReceivedCode{}, Message: "正在申请资源"}
	if in.Kind == "email" {
		o.QueueMinutes = queueMinutes
	}
	tx, err := a.db.Begin()
	if err != nil {
		a.databaseError(w, err)
		return
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO orders(id,cdk_id,kind,status,created_at,expires_at,cancel_after,attempt,max_attempts,message,admin_request_id) VALUES(?,NULL,?,?,?,?,?,0,0,?,?)`, o.ID, o.Kind, o.Status, millis(now), millis(o.ExpiresAt), millis(now), o.Message, in.RequestID)
	if err == nil {
		_, err = tx.Exec("INSERT INTO admin_resource_requests(request_id,kind,order_id) VALUES(?,?,?)", in.RequestID, in.Kind, o.ID)
	}
	if err == nil {
		err = insertAllocationQueue(tx, o, request)
	}
	if err == nil {
		err = insertPhoneChannel(tx, o.ID, selectedChannel)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		a.databaseError(w, err)
		return
	}
	a.audit("admin_allocate_resource", o.ID, in.Kind)
	a.allocateOrder(w, o, request)
}

// Only use this response after the request and active-order lookups, before an
// allocation intent is persisted or a purchase is sent to the provider. It lets
// the browser discard a rejected pending request safely and submit a new choice.
// Conflicts involving accepted request IDs and uncertain outcomes must not carry
// this code: those retries must keep their original idempotency key.
func adminAllocationRejected(w http.ResponseWriter, err error) {
	respond(w, http.StatusConflict, map[string]string{
		"error": err.Error(),
		"code":  "admin_allocation_rejected",
	})
}

func (a *App) validateAdminResource(kind string, s Settings) error {
	if !a.providerReady() {
		return errors.New("请先在后台设置中保存 SMSBower API 密钥")
	}
	// The public redemption switches and voucher limits do not restrict an
	// administrator. Provider pricing, resource parameters and TTL still apply.
	if err := validateSettings(s); err != nil {
		return err
	}
	if a.opts.Mode == "live" {
		if kind == "phone" && (s.PhoneMaxPrice == "" || s.PhoneCountry == "") {
			return errors.New("请先配置手机号国家和价格上限")
		}
		if kind == "email" && (s.EmailMaxPrice == "" || s.EmailDomain == "") {
			return errors.New("请先配置邮箱域名和价格上限")
		}
	}
	return nil
}

// SQLite cannot remove NOT NULL in place. Rebuild the order table in one
// transaction after the quota migration, keeping order IDs, row ordering,
// receipts, all existing indexes and triggers intact.
func migrateAdminResources(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query("PRAGMA table_info(orders)")
	if err != nil {
		return err
	}
	cdkRequired, hasRequestID := false, false
	for rows.Next() {
		var ordinal, required, primary int
		var name, kind string
		var defaultValue any
		if err = rows.Scan(&ordinal, &name, &kind, &required, &defaultValue, &primary); err != nil {
			rows.Close()
			return err
		}
		if name == "cdk_id" {
			cdkRequired = required != 0
		}
		hasRequestID = hasRequestID || name == "admin_request_id"
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !hasRequestID {
		if _, err = tx.Exec("ALTER TABLE orders ADD COLUMN admin_request_id TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	if cdkRequired {
		rows, err = tx.Query("SELECT sql FROM sqlite_master WHERE tbl_name='orders' AND type IN ('index','trigger') AND sql IS NOT NULL")
		if err != nil {
			return err
		}
		var schema []string
		for rows.Next() {
			var statement string
			if err = rows.Scan(&statement); err != nil {
				rows.Close()
				return err
			}
			schema = append(schema, statement)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		_, err = tx.Exec(`CREATE TABLE orders_admin_upgrade (
 id TEXT PRIMARY KEY, cdk_id TEXT REFERENCES cdks(id), kind TEXT NOT NULL, status TEXT NOT NULL,
 provider_id TEXT NOT NULL DEFAULT '', resource TEXT NOT NULL DEFAULT '', code TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, cancel_after INTEGER NOT NULL,
 attempt INTEGER NOT NULL, max_attempts INTEGER NOT NULL, message TEXT NOT NULL DEFAULT '',
 cancel_reason TEXT NOT NULL DEFAULT '', last_poll INTEGER NOT NULL DEFAULT 0,
 mail_round INTEGER NOT NULL DEFAULT 1, codes TEXT NOT NULL DEFAULT '[]',
 usage_counted INTEGER NOT NULL DEFAULT 0, round_wait_seen INTEGER NOT NULL DEFAULT 0,
 admin_request_id TEXT NOT NULL DEFAULT '');`)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO orders_admin_upgrade(rowid," + orderColumns + ") SELECT rowid," + orderColumns + " FROM orders"); err != nil {
			return err
		}
		if _, err = tx.Exec("DROP TABLE orders; ALTER TABLE orders_admin_upgrade RENAME TO orders"); err != nil {
			return err
		}
		for _, statement := range schema {
			if _, err = tx.Exec(statement); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS admin_resource_requests (
 request_id TEXT PRIMARY KEY, kind TEXT NOT NULL, order_id TEXT NOT NULL REFERENCES orders(id));
CREATE INDEX IF NOT EXISTS admin_resource_history_idx ON orders(kind,created_at DESC) WHERE cdk_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS admin_resource_active_idx ON orders(kind)
 WHERE cdk_id IS NULL AND status NOT IN ('completed','cancelled','expired','failed');`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
