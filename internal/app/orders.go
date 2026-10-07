package app

import (
	"database/sql"
	"errors"
	"net/http"
	"openai-mail-transaction/internal/provider"
	"sync"
	"time"
)

func (a *App) redeem(w http.ResponseWriter, r *http.Request) {
	a.providerGate.RLock()
	defer a.providerGate.RUnlock()
	if a.updateMaintenance.Load() {
		fail(w, http.StatusServiceUnavailable, "正在更新程序，请稍后重试")
		return
	}
	var in struct {
		phoneSelection
		CDK          string `json:"cdk"`
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
	code := normalizeCDK(in.CDK)
	if len(code) < 5 || len(code) > 100 {
		fail(w, 400, "请输入有效兑换码")
		return
	}
	c, err := scanCDK(a.db.QueryRow("SELECT "+cdkColumns+" FROM cdks WHERE hash=?", hash(code)))
	if err == sql.ErrNoRows {
		fail(w, 404, "兑换码不存在")
		return
	}
	if err != nil {
		a.databaseError(w, err)
		return
	}
	unlock := a.lock(c.ID)
	defer unlock()
	c, err = a.getCDK(c.ID)
	if err != nil {
		a.databaseError(w, err)
		return
	}
	if c.CodeCipher == "" {
		if err = a.rememberCDKCode(c.ID, code); err != nil {
			a.databaseError(w, err)
			return
		}
	}
	a.redeemLocked(w, c, queueMinutes, in.phoneSelection)
}

// The caller holds providerGate and the voucher lock. A voucher owns at most
// one unsettled activation, regardless of how many successful uses remain.
func (a *App) redeemLocked(w http.ResponseWriter, c CDK, queueMinutes int, choice phoneSelection) {
	if c.Status == "disabled" {
		fail(w, 409, "兑换码已停用")
		return
	}
	previous, previousErr := a.latestOrder(c.ID)
	if previousErr != nil && previousErr != sql.ErrNoRows {
		a.databaseError(w, previousErr)
		return
	}
	if previousErr == nil && (c.Status != "available" || !terminalOrder(previous.Status)) {
		a.orderResponse(w, previous, true)
		return
	}
	if time.Now().After(c.ExpiresAt) {
		if previousErr == nil {
			a.orderResponse(w, previous, true)
		} else {
			fail(w, 410, "兑换码已过期")
		}
		return
	}
	if c.Status != "available" || c.UsedCount >= c.UsageLimit {
		fail(w, 409, "兑换次数已用完")
		return
	}
	if err := a.validateReady(c); err != nil {
		fail(w, 409, err.Error())
		return
	}
	now := time.Now().UTC()
	ttl := time.Duration(c.Snapshot.PhoneTTLMinutes) * time.Minute
	if c.Kind == "email" {
		ttl = time.Duration(c.Snapshot.EmailTTLMinutes) * time.Minute
	}
	request := provider.Request{Kind: c.Kind, TTL: ttl}
	var selectedChannel *provider.PhoneChannel
	if c.Kind == "phone" {
		var err error
		request, selectedChannel, err = a.selectedPhoneRequest(a.effectivePhoneSettings(c.Snapshot), choice)
		if err != nil {
			fail(w, 409, err.Error())
			return
		}
	} else {
		request.Service = c.Snapshot.EmailService
		request.Domain = c.Snapshot.EmailDomain
		request.MaxPrice = c.Snapshot.EmailMaxPrice
	}
	// A live channel lookup can outlast the remaining voucher lifetime.
	if !time.Now().Before(c.ExpiresAt) {
		fail(w, 410, "兑换码已过期")
		return
	}
	now = time.Now().UTC()
	o := Order{ID: randomString(12), CDKID: c.ID, Kind: c.Kind, Status: "allocating", CreatedAt: now, ExpiresAt: now.Add(ttl), CancelAfter: now, Attempt: c.Attempts + 1, MaxAttempts: c.MaxAttempts, MailRound: 1, Codes: []ReceivedCode{}, Message: "正在申请资源"}
	if c.Kind == "email" {
		o.QueueMinutes = queueMinutes
	}
	tx, err := a.db.Begin()
	if err != nil {
		a.databaseError(w, err)
		return
	}
	defer tx.Rollback()
	result, err := tx.Exec("UPDATE cdks SET status='active', attempts=attempts+1 WHERE id=? AND status='available' AND used_count<usage_limit", c.ID)
	if err != nil {
		a.databaseError(w, err)
		return
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		fail(w, 409, "兑换正在处理中，请稍后重试")
		return
	}
	_, err = tx.Exec(`INSERT INTO orders(id,cdk_id,kind,status,created_at,expires_at,cancel_after,attempt,max_attempts,message) VALUES(?,?,?,?,?,?,?,?,?,?)`, o.ID, o.CDKID, o.Kind, o.Status, millis(now), millis(o.ExpiresAt), millis(now), o.Attempt, o.MaxAttempts, o.Message)
	if err != nil {
		a.databaseError(w, err)
		return
	}
	if err = insertAllocationQueue(tx, o, request); err != nil {
		a.databaseError(w, err)
		return
	}
	if err = insertPhoneChannel(tx, o.ID, selectedChannel); err != nil {
		a.databaseError(w, err)
		return
	}
	if err = tx.Commit(); err != nil {
		a.databaseError(w, err)
		return
	}
	a.allocateOrder(w, o, request)
}

// Both administrator and voucher purchases use the same durable transition.
func (a *App) allocateOrder(w http.ResponseWriter, o Order, request provider.Request) {
	if err := a.performAllocation(&o, request); err != nil {
		a.databaseError(w, err)
		return
	}
	a.orderResponse(w, o, true)
}

func terminalOrder(status string) bool {
	return status == "completed" || status == "cancelled" || status == "expired" || status == "failed"
}

func (a *App) replaceOrder(w http.ResponseWriter, r *http.Request) {
	a.providerGate.RLock()
	defer a.providerGate.RUnlock()
	if a.updateMaintenance.Load() {
		fail(w, http.StatusServiceUnavailable, "正在更新程序，请稍后重试")
		return
	}
	o, err := a.actionOrder(r)
	if err != nil {
		fail(w, 401, err.Error())
		return
	}
	var in struct {
		phoneSelection
		QueueMinutes *int `json:"queue_minutes"`
	}
	// Older clients submit an empty body for replacement.
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	queueMinutes, err := requestedQueueMinutes(in.QueueMinutes)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	unlock := a.lock(orderLockKey(o))
	defer unlock()
	latest, err := a.latestOrder(o.CDKID)
	if err != nil {
		a.databaseError(w, err)
		return
	}
	// A retried request carrying the old token resumes the already allocated
	// resource instead of spending another purchase.
	if latest.ID != o.ID {
		a.orderResponse(w, latest, true)
		return
	}
	if !terminalOrder(latest.Status) {
		fail(w, 409, "请先取消或结束当前资源")
		return
	}
	c, err := a.getCDK(o.CDKID)
	if err != nil {
		a.databaseError(w, err)
		return
	}
	a.redeemLocked(w, c, queueMinutes, in.phoneSelection)
}

func (a *App) nextCode(w http.ResponseWriter, r *http.Request) {
	a.providerGate.RLock()
	defer a.providerGate.RUnlock()
	var in struct {
		Round int `json:"round"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Round < 1 || in.Round > 3 {
		fail(w, 400, "邮件轮次无效")
		return
	}
	o, err := a.actionOrder(r)
	if err != nil {
		fail(w, 401, err.Error())
		return
	}
	unlock := a.lock(orderLockKey(o))
	defer unlock()
	o, err = a.getOrder(o.ID)
	if err != nil {
		a.databaseError(w, err)
		return
	}
	if o.Kind != "email" {
		fail(w, 409, "该资源不支持继续接收邮件")
		return
	}
	if o.MailRound != in.Round {
		a.orderResponse(w, o, false)
		return
	}
	if o.Status != "received" || o.Code == "" || o.MailRound >= 3 || !time.Now().Before(o.ExpiresAt) {
		fail(w, 409, "当前邮箱不能继续接收下一封")
		return
	}
	client, ok := a.currentClient().(provider.NextCodeClient)
	if !ok {
		fail(w, 409, "资源服务暂不支持继续接收")
		return
	}
	if err = a.startNextCode(&o, client); err != nil {
		a.databaseError(w, err)
		return
	}
	a.orderResponse(w, o, false)
}

// The manual endpoint and automatic worker share one durable intent and the
// same order lock. Neither a second request nor restart replays that intent.
func (a *App) startNextCode(o *Order, client provider.NextCodeClient) error {
	previousCode := o.Code
	previousWaitSeen := o.RoundWaitSeen
	o.MailRound++
	o.RoundWaitSeen = false
	o.Code = ""
	o.Status = "next_pending"
	o.Message = "正在请求下一封"
	o.LastPoll = time.Now().UTC()
	if err := a.saveOrder(o, "active"); err != nil {
		return err
	}
	ctx, cancel := a.providerContext()
	err := client.NextCode(ctx, o.Kind, o.ProviderID)
	cancel()
	if err != nil {
		var pe *provider.Error
		if errors.As(err, &pe) && !pe.Uncertain {
			o.MailRound--
			o.RoundWaitSeen = previousWaitSeen
			o.Code, o.Status, o.Message = previousCode, "received", safeError(err)
		} else {
			o.Status = "next_uncertain"
			o.Message = "接码状态待确认，请稍后刷新"
		}
	} else {
		o.Status, o.Message = "waiting", "等待下一封验证码"
	}
	cdkStatus := "active"
	if o.Status == "next_uncertain" {
		cdkStatus = "review"
	}
	return a.saveOrder(o, cdkStatus)
}

func (a *App) pollProvider(o *Order) (provider.Result, error) {
	ctx, cancel := a.providerContext()
	defer cancel()
	client := a.currentClient()
	if rounds, ok := client.(provider.RoundPollClient); ok {
		return rounds.PollRound(ctx, o.Kind, o.ProviderID, o.MailRound)
	}
	return client.Poll(ctx, o.Kind, o.ProviderID)
}

func (a *App) currentOrder(w http.ResponseWriter, r *http.Request) {
	a.providerGate.RLock()
	defer a.providerGate.RUnlock()
	o, err := a.actionOrder(r)
	if err != nil {
		fail(w, 401, err.Error())
		return
	}
	unlock := a.lock(orderLockKey(o))
	defer unlock()
	o, err = a.getOrder(o.ID)
	if err != nil {
		a.databaseError(w, err)
		return
	}
	if err = a.refresh(&o); err != nil {
		a.databaseError(w, err)
		return
	}
	a.orderResponse(w, o, false)
}
func (a *App) cancelOrder(w http.ResponseWriter, r *http.Request) {
	a.providerGate.RLock()
	defer a.providerGate.RUnlock()
	o, err := a.actionOrder(r)
	if err != nil {
		fail(w, 401, err.Error())
		return
	}
	unlock := a.lock(orderLockKey(o))
	defer unlock()
	o, err = a.getOrder(o.ID)
	if err != nil {
		a.databaseError(w, err)
		return
	}
	if o.Status == "queued" {
		if err = a.finishUnallocatedOrder(&o, "cancelled", "已取消排队"); err != nil {
			a.databaseError(w, err)
			return
		}
		a.orderResponse(w, o, false)
		return
	}
	if !a.providerReady() {
		fail(w, 503, "资源服务尚未配置，请联系管理员")
		return
	}
	if o.Status != "waiting" && o.Status != "cancel_pending" {
		fail(w, 409, "当前订单不可取消")
		return
	}
	if o.UsageCounted || len(o.Codes) > 0 {
		if err = a.finish(&o); err != nil {
			a.databaseError(w, err)
			return
		}
		a.orderResponse(w, o, false)
		return
	}
	if time.Now().Before(o.CancelAfter) {
		// Accept the user's intent immediately, while retaining the provider's
		// minimum cancellation window. The worker continues polling for late
		// codes and retries the release without another browser action.
		o.CancelReason = "cancelled"
		if err = a.requestCancel(&o); err != nil {
			a.databaseError(w, err)
			return
		}
		a.orderResponse(w, o, false)
		return
	}
	// Recheck for a code immediately before cancelling. A confirmed receipt spends
	// the voucher even if a browser has not fetched that code yet.
	result, pollErr := a.pollProvider(&o)
	if pollErr == nil && result.Status != "waiting" {
		if err = a.applyResult(&o, result); err != nil {
			a.databaseError(w, err)
			return
		}
		if o.Status != "waiting" && o.Status != "cancel_pending" {
			a.orderResponse(w, o, false)
			return
		}
	}
	o.CancelReason = "cancelled"
	if err = a.requestCancel(&o); err != nil {
		a.databaseError(w, err)
		return
	}
	a.orderResponse(w, o, false)
}
func (a *App) completeOrder(w http.ResponseWriter, r *http.Request) {
	a.providerGate.RLock()
	defer a.providerGate.RUnlock()
	if !a.providerReady() {
		fail(w, 503, "资源服务尚未配置，请联系管理员")
		return
	}
	o, err := a.actionOrder(r)
	if err != nil {
		fail(w, 401, err.Error())
		return
	}
	unlock := a.lock(orderLockKey(o))
	defer unlock()
	o, err = a.getOrder(o.ID)
	if err != nil {
		a.databaseError(w, err)
		return
	}
	if o.Status == "completed" {
		a.orderResponse(w, o, false)
		return
	}
	if (!o.UsageCounted && len(o.Codes) == 0 && o.Code == "") || terminalOrder(o.Status) || o.Status == "review" {
		fail(w, 409, "收到验证码后才能完成")
		return
	}
	if err = a.finish(&o); err != nil {
		a.databaseError(w, err)
		return
	}
	a.orderResponse(w, o, false)
}
func (a *App) applyResult(o *Order, result provider.Result) error {
	o.LastPoll = time.Now().UTC()
	switch result.Status {
	case "received":
		if result.Code == "" {
			o.Message = "上游验证码为空，正在重试"
			return a.saveOrder(o, "")
		}
		// getCode may temporarily repeat the previous result after status=5.
		// It is neither a new mail nor another successful resource use.
		for _, received := range o.Codes {
			if received.Round == o.MailRound {
				return nil
			}
		}
		_, hasRoundIdentity := a.currentClient().(provider.RoundPollClient)
		if !hasRoundIdentity && !o.RoundWaitSeen && len(o.Codes) > 0 && o.Codes[len(o.Codes)-1].Code == result.Code {
			return nil
		}
		if o.MailRound < 1 {
			o.MailRound = 1
		}
		o.Codes = append(o.Codes, ReceivedCode{Round: o.MailRound, Code: result.Code, ReceivedAt: time.Now().UTC()})
		o.UsageCounted = true
		o.Code = result.Code
		o.Status = "received"
		o.Message = "验证码已收到"
		return a.saveOrder(o, "active")
	case "cancelled", "expired":
		if o.UsageCounted || len(o.Codes) > 0 || o.Code != "" {
			o.Status = "completed"
			o.Message = "已完成"
			return a.release(o)
		}
		o.Status = result.Status
		if o.CancelReason == "expired" {
			o.Status = "expired"
		}
		o.Message = "资源已释放"
		return a.release(o)
	case "waiting":
		if o.MailRound > 1 && !o.RoundWaitSeen {
			o.RoundWaitSeen = true
			return a.saveOrder(o, "")
		}
		return nil
	default:
		o.Message = "上游状态待确认"
		return a.saveOrder(o, "")
	}
}
func (a *App) release(o *Order) error {
	return a.saveOrder(o, "available")
}
func (a *App) requestCancel(o *Order) error {
	if o.UsageCounted || len(o.Codes) > 0 {
		return a.finish(o)
	}
	o.Status = "cancel_pending"
	o.Message = "正在确认资源释放"
	o.LastPoll = time.Now().UTC()
	if err := a.saveOrder(o, ""); err != nil {
		return err
	}
	if time.Now().Before(o.CancelAfter) {
		return nil
	}
	ctx, cancel := a.providerContext()
	err := a.currentClient().Cancel(ctx, o.Kind, o.ProviderID)
	cancel()
	if err != nil {
		o.Message = safeError(err)
		var pe *provider.Error
		if errors.As(err, &pe) && pe.RetryAfter > 0 {
			o.CancelAfter = time.Now().UTC().Add(pe.RetryAfter)
		}
		if time.Now().After(o.ExpiresAt.Add(30 * time.Minute)) {
			o.Status = "review"
			o.Message = "释放结果待核对，请联系管理员"
			return a.saveOrder(o, "review")
		}
		return a.saveOrder(o, "")
	}
	o.Status = o.CancelReason
	if o.Status != "expired" {
		o.Status = "cancelled"
	}
	o.Message = "资源已释放"
	return a.release(o)
}
func (a *App) refresh(o *Order) error {
	if !a.providerReady() {
		o.Message = "资源服务尚未配置，请联系管理员"
		return nil
	}
	now := time.Now().UTC()
	if now.Sub(o.LastPoll) < 5*time.Second {
		return nil
	}
	// The lock excludes an in-flight HTTP action. A remaining durable intent
	// therefore means its final write was lost; poll/reconcile without replaying it.
	if o.Status == "allocating" {
		o.Status, o.Message = "review", "购买结果待核对，请联系管理员"
		return a.saveOrder(o, "review")
	}
	if o.Status == "next_pending" {
		o.Status, o.Message = "next_uncertain", "接码状态待确认，请稍后刷新"
		if err := a.saveOrder(o, "review"); err != nil {
			return err
		}
	}
	if o.Status == "complete_pending" {
		return a.finish(o)
	}
	if o.Status == "received" {
		if now.After(o.ExpiresAt) {
			return a.finish(o)
		}
		return nil
	}
	if o.Status != "waiting" && o.Status != "cancel_pending" && o.Status != "next_uncertain" {
		return nil
	}
	result, err := a.pollProvider(o)
	o.LastPoll = now
	if err == nil {
		if err = a.applyResult(o, result); err != nil {
			return err
		}
		if o.Status != "waiting" && o.Status != "cancel_pending" && o.Status != "next_uncertain" {
			return nil
		}
	} else {
		o.Message = safeError(err)
	}
	if o.Status == "cancel_pending" {
		return a.requestCancel(o)
	}
	if now.After(o.ExpiresAt) {
		o.CancelReason = "expired"
		return a.requestCancel(o)
	}
	return a.saveOrder(o, "")
}
func (a *App) worker() {
	defer a.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
		}
		a.exports.Range(func(key, value any) bool {
			if time.Now().After(value.(codeExport).ExpiresAt) {
				a.exports.Delete(key)
			}
			return true
		})
		now := time.Now()
		rows, err := a.db.Query("SELECT "+orderColumns+` FROM orders WHERE
 (status IN ('waiting','cancel_pending','next_pending','next_uncertain','complete_pending')
 OR (status='allocating' AND NOT EXISTS(SELECT 1 FROM order_allocation_queue q WHERE q.order_id=orders.id))
 OR (status='received' AND (expires_at<=? OR (kind='email' AND mail_round<3
 AND NOT EXISTS(SELECT 1 FROM cdks c WHERE c.id=orders.cdk_id AND c.status='disabled')
 AND NOT EXISTS(SELECT 1 FROM order_auto_mail m WHERE m.order_id=orders.id AND m.round=orders.mail_round AND (m.attempts>=? OR m.next_attempt_at>?))))))
 AND last_poll<=? ORDER BY last_poll LIMIT 100`, now.UnixMilli(), autoMailMaxAttempts, now.UnixMilli(), now.Add(-5*time.Second).UnixMilli())
		if err != nil {
			a.log.Printf("sweep query failed: %v", err)
			continue
		}
		var orders []Order
		for rows.Next() {
			o, e := scanOrder(rows)
			if e == nil {
				orders = append(orders, o)
			}
		}
		rows.Close()
		sem := make(chan struct{}, 4)
		var batch sync.WaitGroup
		for _, item := range orders {
			if a.ctx.Err() != nil {
				break
			}
			sem <- struct{}{}
			batch.Add(1)
			go func(o Order) {
				defer batch.Done()
				defer func() { <-sem }()
				e := a.processActiveOrder(o.ID)
				if e != nil {
					a.transitionError(&o, e)
				}
			}(item)
		}
		batch.Wait()
	}
}
