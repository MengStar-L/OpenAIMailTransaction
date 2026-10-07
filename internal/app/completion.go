package app

import (
	"database/sql"
	"errors"
	"time"

	"openai-mail-transaction/internal/provider"
)

const completionRetryWindow = 2 * time.Minute

type completionState struct {
	StartedAt, NextAttemptAt time.Time
	Attempts                 int
	LastError                string
}

// Keep completion intent separate from receipt history. Existing pending orders
// begin their bounded reconciliation window on their first upgraded refresh.
func migrateCompletionState(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS order_completions (
 order_id TEXT PRIMARY KEY REFERENCES orders(id), started_at INTEGER NOT NULL,
 next_attempt_at INTEGER NOT NULL DEFAULT 0, attempts INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT '');`)
	return err
}

func (a *App) completionState(id string) (completionState, error) {
	_, err := a.db.Exec("INSERT INTO order_completions(order_id,started_at) VALUES(?,?) ON CONFLICT(order_id) DO NOTHING", id, time.Now().UTC().UnixMilli())
	if err != nil {
		return completionState{}, err
	}
	var state completionState
	var started, next int64
	err = a.db.QueryRow("SELECT started_at,next_attempt_at,attempts,last_error FROM order_completions WHERE order_id=?", id).Scan(&started, &next, &state.Attempts, &state.LastError)
	state.StartedAt, state.NextAttemptAt = stamp(started), stamp(next)
	return state, err
}

func completionExpired(o *Order, state completionState, now time.Time) bool {
	return !now.Before(state.StartedAt.Add(completionRetryWindow)) || (!o.ExpiresAt.IsZero() && now.After(o.ExpiresAt.Add(30*time.Minute)))
}

// Only machine codes are logged, never provider URLs, API keys or raw responses.
func completionErrorCode(err error) string {
	var pe *provider.Error
	if errors.As(err, &pe) {
		switch pe.Code {
		case "bad_key", "invalid_request", "upstream_configuration", "activation_not_found", "bad_status", "no_balance", "no_stock", "price_unavailable", "request_failed", "upstream_unavailable", "upstream_http", "invalid_response", "early_cancel":
			return pe.Code
		}
	}
	return "upstream_error"
}

func definitiveCompletionError(err error) bool {
	switch completionErrorCode(err) {
	case "bad_key", "invalid_request", "upstream_configuration", "activation_not_found", "bad_status", "no_balance", "no_stock", "price_unavailable":
		return true
	}
	return false
}

func (a *App) completionReview(o *Order, message string) error {
	o.Status, o.Message, o.LastPoll = "review", message, time.Now().UTC()
	a.log.Printf("completion requires review order=%s", o.ID)
	return a.saveOrder(o, "review")
}

func (a *App) completionFailure(o *Order, state completionState, err error) error {
	now := time.Now().UTC()
	code := completionErrorCode(err)
	if state.LastError != code {
		a.log.Printf("completion confirmation failed order=%s provider_error=%s", o.ID, code)
	}
	next := now.Add(5 * time.Second)
	var pe *provider.Error
	if errors.As(err, &pe) && pe.RetryAfter > 5*time.Second {
		next = now.Add(pe.RetryAfter)
	}
	if _, saveErr := a.db.Exec("UPDATE order_completions SET next_attempt_at=?,last_error=? WHERE order_id=?", millis(next), code, o.ID); saveErr != nil {
		return saveErr
	}
	if definitiveCompletionError(err) {
		return a.completionReview(o, "验证码已保留，完成状态待核对："+safeError(err))
	}
	if completionExpired(o, state, now) {
		return a.completionReview(o, "验证码已保留，完成确认超时，请联系管理员核对")
	}
	o.Status, o.Message, o.LastPoll = "complete_pending", "验证码已保留，正在重试确认："+safeError(err), now
	return a.saveOrder(o, "active")
}

// A received code alone never proves completion. Only an explicit terminal
// response from a provider's read-only status endpoint settles pending intent.
func (a *App) reconcileCompletion(o *Order) (bool, error) {
	client, ok := a.currentClient().(provider.CompletionStatusClient)
	if !ok || o.Kind != "email" {
		return false, nil
	}
	ctx, cancel := a.providerContext()
	result, err := client.CompletionStatus(ctx, o.Kind, o.ProviderID)
	cancel()
	if err != nil {
		return false, err
	}
	if result.Status != "completed" {
		return false, nil
	}
	o.Status, o.Message, o.LastPoll = "completed", "已完成", time.Now().UTC()
	return true, a.release(o)
}

func (a *App) finish(o *Order) error {
	state, err := a.completionState(o.ID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if o.Status == "complete_pending" {
		// Honor provider throttling while ensuring a long Retry-After cannot
		// extend the durable two-minute confirmation deadline.
		if now.Before(state.NextAttemptAt) && !completionExpired(o, state, now) {
			o.LastPoll = now
			return a.saveOrder(o, "active")
		}
		if settled, statusErr := a.reconcileCompletion(o); settled {
			return statusErr
		} else if statusErr != nil {
			return a.completionFailure(o, state, statusErr)
		}
		if completionExpired(o, state, time.Now().UTC()) {
			return a.completionReview(o, "验证码已保留，完成确认超时，请联系管理员核对")
		}
	}
	o.Status, o.Message, o.LastPoll = "complete_pending", "正在确认接码结束", now
	if err = a.saveOrder(o, "active"); err != nil {
		return err
	}
	// Persist each attempt before making the state-changing request. A restart
	// keeps the original deadline and reconciles status before another attempt.
	if _, err = a.db.Exec("UPDATE order_completions SET attempts=attempts+1,next_attempt_at=? WHERE order_id=?", millis(now.Add(5*time.Second)), o.ID); err != nil {
		return err
	}
	ctx, cancel := a.providerContext()
	err = a.currentClient().Complete(ctx, o.Kind, o.ProviderID)
	cancel()
	o.LastPoll = time.Now().UTC()
	if err != nil {
		// An explicit rejection can mean a prior acknowledgment was lost.
		// Check its actual status once before asking for manual review.
		if definitiveCompletionError(err) {
			if settled, statusErr := a.reconcileCompletion(o); settled {
				return statusErr
			}
		}
		return a.completionFailure(o, state, err)
	}
	o.Status, o.Message = "completed", "已完成"
	return a.release(o)
}
