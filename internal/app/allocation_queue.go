package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"openai-mail-transaction/internal/provider"
)

const (
	allocationQueueDefaultMinutes = 10
	allocationQueueMinMinutes     = 1
	allocationQueueMaxMinutes     = 1440
	allocationQueueTTL            = allocationQueueDefaultMinutes * time.Minute
	allocationQueueRetry          = 5 * time.Second
)

type allocationQueue struct {
	Request       provider.Request
	ExpiresAt     time.Time
	NextAttemptAt time.Time
	QueuedAt      time.Time
	Attempts      int
	Minutes       int
}

func requestedQueueMinutes(value *int) (int, error) {
	if value == nil {
		return allocationQueueDefaultMinutes, nil
	}
	if *value < allocationQueueMinMinutes || *value > allocationQueueMaxMinutes {
		return 0, errors.New("排队时间应为 1 至 1440 分钟")
	}
	return *value, nil
}

func migrateAllocationQueue(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS order_allocation_queue (
 order_id TEXT PRIMARY KEY REFERENCES orders(id), request_json TEXT NOT NULL,
 expires_at INTEGER NOT NULL, next_attempt_at INTEGER NOT NULL, attempts INTEGER NOT NULL,
 queued_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS allocation_queue_due_idx ON order_allocation_queue(next_attempt_at);`)
	if err != nil {
		return err
	}
	// Existing releases already use the terminal-state exclusion. Retain that
	// index, but upgrade a missing or older explicit active-state predicate.
	var indexSQL string
	err = tx.QueryRow("SELECT sql FROM sqlite_master WHERE type='index' AND name='admin_resource_active_idx'").Scan(&indexSQL)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	normalized := strings.ToLower(strings.Join(strings.Fields(indexSQL), ""))
	if !strings.Contains(normalized, "wherecdk_idisnullandstatusnotin('completed','cancelled','expired','failed')") {
		if _, err = tx.Exec(`DROP INDEX IF EXISTS admin_resource_active_idx;
CREATE UNIQUE INDEX admin_resource_active_idx ON orders(kind)
 WHERE cdk_id IS NULL AND status NOT IN ('completed','cancelled','expired','failed');`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Store the original price, service, domain and lifetime in the same transaction
// as the initial order intent. Queue retries never reread editable settings.
func insertAllocationQueue(tx *sql.Tx, o Order, request provider.Request) error {
	if request.Kind != "email" {
		return nil
	}
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	minutes := o.QueueMinutes
	if minutes == 0 {
		minutes = allocationQueueDefaultMinutes
	}
	_, err = tx.Exec(`INSERT INTO order_allocation_queue(order_id,request_json,expires_at,next_attempt_at,attempts) VALUES(?,?,?,?,1)`, o.ID, string(data), millis(o.CreatedAt.Add(time.Duration(minutes)*time.Minute)), millis(o.CreatedAt))
	return err
}

func (a *App) getAllocationQueue(id string) (allocationQueue, error) {
	var q allocationQueue
	var data string
	var expires, next, queued, created int64
	err := a.db.QueryRow(`SELECT q.request_json,q.expires_at,q.next_attempt_at,q.attempts,q.queued_at,o.created_at FROM order_allocation_queue q JOIN orders o ON o.id=q.order_id WHERE order_id=?`, id).Scan(&data, &expires, &next, &q.Attempts, &queued, &created)
	if err != nil {
		return q, err
	}
	q.ExpiresAt, q.NextAttemptAt = stamp(expires), stamp(next)
	q.QueuedAt = stamp(queued)
	// Duration is encoded by the existing fixed deadline, preserving legacy
	// five-minute queues without a migration that would extend their wait.
	q.Minutes = int((expires - created + time.Minute.Milliseconds() - 1) / time.Minute.Milliseconds())
	err = json.Unmarshal([]byte(data), &q.Request)
	return q, err
}

func (a *App) decorateQueue(o *Order) {
	if o.Kind != "email" {
		return
	}
	q, err := a.getAllocationQueue(o.ID)
	if err == nil {
		o.QueueMinutes = q.Minutes
		if !q.QueuedAt.IsZero() {
			o.QueueExpiresAt, o.QueueNextAttemptAt, o.QueueAttempts = &q.ExpiresAt, &q.NextAttemptAt, q.Attempts
		}
	}
}

// A duration change and any resulting local expiry share the purchase lock and
// transaction. The deadline is always anchored to the original order intent,
// so retries and page refreshes cannot silently grant another full wait.
func (a *App) updateQueueWait(w http.ResponseWriter, r *http.Request) {
	a.providerGate.RLock()
	defer a.providerGate.RUnlock()
	o, err := a.actionOrder(r)
	if err != nil {
		fail(w, 401, err.Error())
		return
	}
	var in struct {
		QueueMinutes *int `json:"queue_minutes"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.QueueMinutes == nil {
		fail(w, 400, "请填写排队时间")
		return
	}
	minutes, err := requestedQueueMinutes(in.QueueMinutes)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	unlock := a.lock(orderLockKey(o))
	defer unlock()
	o, err = a.getOrder(o.ID)
	if err != nil {
		a.databaseError(w, err)
		return
	}
	if o.Kind != "email" || o.Status != "queued" {
		fail(w, 409, "当前资源不在排队中")
		return
	}
	q, err := a.getAllocationQueue(o.ID)
	if err != nil {
		a.databaseError(w, err)
		return
	}
	now := time.Now().UTC()
	// An already elapsed queue cannot be revived in the interval before the
	// background worker records its terminal state.
	if !now.Before(q.ExpiresAt) {
		if err = a.finishUnallocatedOrder(&o, "expired", "排队已超时，请重试"); err != nil {
			a.databaseError(w, err)
			return
		}
		a.orderResponse(w, o, false)
		return
	}
	expires := o.CreatedAt.Add(time.Duration(minutes) * time.Minute)
	next := q.NextAttemptAt
	if next.After(expires) {
		next = expires
	}
	tx, err := a.db.Begin()
	if err != nil {
		a.databaseError(w, err)
		return
	}
	defer tx.Rollback()
	_, err = tx.Exec("UPDATE order_allocation_queue SET expires_at=?,next_attempt_at=? WHERE order_id=?", millis(expires), millis(next), o.ID)
	expired := !now.Before(expires)
	if err == nil && expired {
		err = finishUnallocatedOrderTx(tx, o, "expired", "排队已超时，请重试")
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		a.databaseError(w, err)
		return
	}
	if expired {
		o.Status, o.Message = "expired", "排队已超时，请重试"
	}
	a.orderResponse(w, o, false)
}

// A definite rejection or local queue cancellation never consumed a resource.
// Release the voucher and undo its purchase attempt atomically, preserving an
// administrator's disabled state and any already consumed quota.
func (a *App) finishUnallocatedOrder(o *Order, status, message string) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	err = finishUnallocatedOrderTx(tx, *o, status, message)
	if err == nil {
		err = tx.Commit()
	}
	if err == nil {
		o.Status, o.Message = status, message
	}
	return err
}

func finishUnallocatedOrderTx(tx *sql.Tx, o Order, status, message string) error {
	_, err := tx.Exec("UPDATE orders SET status=?,message=? WHERE id=?", status, message, o.ID)
	if err == nil && o.CDKID != "" {
		_, err = tx.Exec(`UPDATE cdks SET status=CASE WHEN status='disabled' THEN 'disabled' WHEN used_count>=usage_limit THEN 'used' ELSE 'available' END, attempts=MAX(0,attempts-1) WHERE id=?`, o.CDKID)
	}
	return err
}

func (a *App) enqueueAllocation(o *Order) error {
	q, err := a.getAllocationQueue(o.ID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if !now.Before(q.ExpiresAt) {
		return a.finishUnallocatedOrder(o, "expired", "排队已超时，请重试")
	}
	next := now.Add(allocationQueueRetry)
	if next.After(q.ExpiresAt) {
		next = q.ExpiresAt
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec("UPDATE orders SET status='queued',message='排队中，正在等待可用邮箱' WHERE id=?", o.ID)
	if err == nil {
		_, err = tx.Exec("UPDATE order_allocation_queue SET next_attempt_at=?,queued_at=CASE WHEN queued_at=0 THEN ? ELSE queued_at END WHERE order_id=?", millis(next), millis(now), o.ID)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err == nil {
		o.Status, o.Message = "queued", "排队中，正在等待可用邮箱"
	}
	return err
}

// The caller holds providerGate followed by orderLockKey throughout the upstream
// call and its final durable write, so cancellation cannot discard a purchase.
func (a *App) performAllocation(o *Order, request provider.Request) error {
	started := time.Now()
	var activation provider.Activation
	var allocErr error
	var phoneStockExhausted bool
	if request.Kind == "phone" {
		activation, allocErr, phoneStockExhausted = retryPhoneAllocation(a.ctx, a.currentClient(), request, phoneAllocationWindow, phoneAllocationRetry)
	} else {
		ctx, cancel := a.providerContext()
		activation, allocErr = a.currentClient().Allocate(ctx, request)
		cancel()
	}
	a.observeEmailAllocation(request, allocErr)
	if allocErr != nil {
		var pe *provider.Error
		if errors.As(allocErr, &pe) && !pe.Uncertain {
			if request.Kind == "email" && pe.Code == "no_stock" {
				return a.enqueueAllocation(o)
			}
			if phoneStockExhausted {
				if err := a.markPhoneUnavailable(request); err != nil {
					return err
				}
				return a.finishUnallocatedOrder(o, "failed", phoneUnavailableMessage)
			}
			return a.finishUnallocatedOrder(o, "failed", safeError(allocErr))
		}
		o.Status, o.Message = "review", "购买结果待核对，请联系管理员"
		if err := a.saveOrder(o, "review"); err != nil {
			return err
		}
		a.audit("allocation_uncertain", o.ID, allocationUncertainDetail(request.Kind, allocErr, time.Since(started)))
		return nil
	}
	o.ProviderID, o.Resource = activation.ID, activation.Resource
	o.Status, o.Message = "waiting", "等待验证码"
	if activation.ID == "" || activation.Resource == "" {
		o.Status, o.Message = "review", "资源信息待核对，请联系管理员"
	}
	// Waiting for stock does not shorten the purchased mailbox's lifetime.
	now := time.Now().UTC()
	o.ExpiresAt, o.CancelAfter = now.Add(request.TTL), now
	if !activation.ExpiresAt.IsZero() && activation.ExpiresAt.Before(o.ExpiresAt) {
		o.ExpiresAt = activation.ExpiresAt
	}
	if !activation.CancelAfter.IsZero() {
		o.CancelAfter = activation.CancelAfter
	}
	status := "active"
	if o.Status == "review" {
		status = "review"
	}
	return a.saveOrder(o, status)
}

// Safe to call concurrently. Every due attempt is claimed durably before the
// network call. An interrupted allocating state is reconciled, never replayed.
func (a *App) processQueuedOrder(id string) error {
	if a.ctx.Err() != nil {
		return nil
	}
	a.providerGate.RLock()
	defer a.providerGate.RUnlock()
	o, err := a.getOrder(id)
	if err != nil {
		return err
	}
	unlock := a.lock(orderLockKey(o))
	defer unlock()
	if a.ctx.Err() != nil {
		return nil
	}
	o, err = a.getOrder(id)
	if err != nil || (o.Status != "queued" && o.Status != "allocating") {
		return err
	}
	q, err := a.getAllocationQueue(id)
	if err != nil {
		return err
	}
	if o.Status == "allocating" {
		o.Status, o.Message = "review", "购买结果待核对，请联系管理员"
		return a.saveOrder(&o, "review")
	}
	now := time.Now().UTC()
	if !now.Before(q.ExpiresAt) {
		return a.finishUnallocatedOrder(&o, "expired", "排队已超时，请重试")
	}
	if o.CDKID != "" {
		c, err := a.getCDK(o.CDKID)
		if err != nil {
			return err
		}
		if c.Status == "disabled" {
			return a.finishUnallocatedOrder(&o, "cancelled", "兑换码已停用，排队已取消")
		}
		if !now.Before(c.ExpiresAt) {
			return a.finishUnallocatedOrder(&o, "expired", "兑换码已过期，排队已结束")
		}
		if c.UsedCount >= c.UsageLimit {
			return a.finishUnallocatedOrder(&o, "failed", "兑换次数已用完")
		}
	}
	if !a.providerReady() {
		return a.finishUnallocatedOrder(&o, "failed", "资源服务尚未配置，请联系管理员")
	}
	if now.Before(q.NextAttemptAt) {
		return nil
	}
	if q.Request.Kind != "email" || q.Request.TTL <= 0 {
		return a.finishUnallocatedOrder(&o, "failed", "排队配置无效，请重新获取")
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec("UPDATE orders SET status='allocating',message='正在获取邮箱' WHERE id=? AND status='queued'", id)
	if err == nil {
		_, err = tx.Exec("UPDATE order_allocation_queue SET attempts=attempts+1 WHERE order_id=?", id)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return err
	}
	o.Status, o.Message = "allocating", "正在获取邮箱"
	return a.performAllocation(&o, q.Request)
}

// Separate from receipt polling: a slow allocation cannot occupy its workers.
// Two allocation slots bound provider traffic; Close cancels and joins both.
func (a *App) allocationQueueWorker() {
	defer a.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
		}
		now := time.Now().UnixMilli()
		rows, err := a.db.Query(`SELECT o.id FROM orders o JOIN order_allocation_queue q ON q.order_id=o.id
 WHERE o.status='allocating' OR (o.status='queued' AND (q.next_attempt_at<=? OR q.expires_at<=?))
 ORDER BY q.next_attempt_at LIMIT 100`, now, now)
		if err != nil {
			a.log.Printf("allocation queue query failed: %v", err)
			continue
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		sem := make(chan struct{}, 2)
		var batch sync.WaitGroup
		for _, id := range ids {
			select {
			case <-a.ctx.Done():
				batch.Wait()
				return
			case sem <- struct{}{}:
			}
			batch.Add(1)
			go func(id string) {
				defer batch.Done()
				defer func() { <-sem }()
				if err := a.processQueuedOrder(id); err != nil {
					a.log.Printf("allocation queue order %s failed: %v", id, err)
				}
			}(id)
		}
		batch.Wait()
	}
}
