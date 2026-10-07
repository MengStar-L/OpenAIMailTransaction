package app

import (
	"database/sql"
	"time"

	"openai-mail-transaction/internal/provider"
)

const (
	autoMailMaxAttempts = 3
	autoMailRetryDelay  = 30 * time.Second
)

// The old 20-minute setting was the application default. Upgrade that value
// once, including unpurchased voucher/queue snapshots, without overriding later
// administrator edits. Allocated orders retain their fixed deadline because
// old records cannot distinguish a local deadline from an upstream expiry cap.
func migrateAutomaticMail(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`CREATE TABLE IF NOT EXISTS order_auto_mail (
 order_id TEXT PRIMARY KEY REFERENCES orders(id), round INTEGER NOT NULL,
 attempts INTEGER NOT NULL, next_attempt_at INTEGER NOT NULL);`); err != nil {
		return err
	}
	var applied string
	err = tx.QueryRow("SELECT value FROM metadata WHERE key='email_wait_25_minutes_v1'").Scan(&applied)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == sql.ErrNoRows {
		if _, err = tx.Exec(`UPDATE settings SET value=json_set(value,'$.email_ttl_minutes',25)
 WHERE json_extract(value,'$.email_ttl_minutes')=20;
UPDATE cdks SET snapshot=json_set(snapshot,'$.email_ttl_minutes',25)
 WHERE kind='email' AND json_extract(snapshot,'$.email_ttl_minutes')=20;
UPDATE order_allocation_queue SET request_json=json_set(request_json,'$.TTL',1500000000000)
 WHERE json_extract(request_json,'$.TTL')=1200000000000 AND order_id IN
 (SELECT id FROM orders WHERE kind='email' AND status IN ('queued','allocating') AND provider_id='');
INSERT INTO metadata(key,value) VALUES('email_wait_25_minutes_v1','applied');`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type autoMailState struct {
	Round, Attempts int
	NextAttemptAt   time.Time
}

func (a *App) autoMailState(o *Order) (autoMailState, error) {
	var state autoMailState
	var next int64
	err := a.db.QueryRow("SELECT round,attempts,next_attempt_at FROM order_auto_mail WHERE order_id=?", o.ID).Scan(&state.Round, &state.Attempts, &next)
	if err == sql.ErrNoRows || (err == nil && state.Round != o.MailRound) {
		return autoMailState{Round: o.MailRound}, nil
	}
	state.NextAttemptAt = stamp(next)
	return state, err
}

func (a *App) decorateAutoNext(o *Order) {
	o.AutoNextState, o.AutoNextAt = "", nil
	if !o.CanNextCode {
		return
	}
	if _, ok := a.currentClient().(provider.NextCodeClient); !ok {
		o.AutoNextState = "failed"
		return
	}
	state, err := a.autoMailState(o)
	if err != nil || state.Attempts >= autoMailMaxAttempts {
		o.AutoNextState = "failed"
	} else if state.Attempts > 0 {
		o.AutoNextState, o.AutoNextAt = "retrying", &state.NextAttemptAt
	} else {
		o.AutoNextState = "pending"
	}
}

// Runs only while holding providerGate and the owning voucher/admin lock.
// A durable budget and delay apply only to definite rejections. next_pending
// and next_uncertain are reconciled by polling, never replayed here.
func (a *App) autoContinueMail(o *Order) error {
	if o.Kind != "email" || o.Status != "received" || o.Code == "" || o.MailRound >= 3 || !time.Now().Before(o.ExpiresAt) {
		return nil
	}
	client, ok := a.currentClient().(provider.NextCodeClient)
	if !ok || !a.providerReady() {
		return nil
	}
	if o.CDKID != "" {
		c, err := a.getCDK(o.CDKID)
		if err != nil {
			return err
		}
		if c.Status == "disabled" {
			return nil
		}
	}
	state, err := a.autoMailState(o)
	if err != nil {
		return err
	}
	if state.Attempts >= autoMailMaxAttempts || time.Now().Before(state.NextAttemptAt) {
		return nil
	}
	// Persist the attempt budget before any network mutation. A crash here can
	// spend one retry but cannot cause an extra upstream continuation.
	_, err = a.db.Exec(`INSERT INTO order_auto_mail(order_id,round,attempts,next_attempt_at) VALUES(?,?,?,?)
 ON CONFLICT(order_id) DO UPDATE SET round=excluded.round,attempts=excluded.attempts,next_attempt_at=excluded.next_attempt_at`,
		o.ID, o.MailRound, state.Attempts+1, millis(time.Now().UTC().Add(autoMailRetryDelay)))
	if err != nil {
		return err
	}
	return a.startNextCode(o, client)
}

// Shared by the timer and tests: a closed browser does not stop receiving mail.
func (a *App) processActiveOrder(id string) error {
	a.providerGate.RLock()
	defer a.providerGate.RUnlock()
	o, err := a.getOrder(id)
	if err != nil {
		return err
	}
	unlock := a.lock(orderLockKey(o))
	defer unlock()
	o, err = a.getOrder(id)
	if err != nil {
		return err
	}
	if err = a.refresh(&o); err != nil {
		return err
	}
	return a.autoContinueMail(&o)
}
