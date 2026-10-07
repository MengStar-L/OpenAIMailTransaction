package app

import (
	"context"
	"net/http"
	"time"

	"openai-mail-transaction/internal/provider"
)

const balanceSuccessTTL = 15 * time.Second
const balanceFailureTTL = 5 * time.Second
const balanceRefreshInterval = 2 * time.Second
const balanceTimeout = 8 * time.Second

type balanceView struct {
	Balance    *string    `json:"balance"`
	Currency   string     `json:"currency"`
	Mode       string     `json:"mode"`
	Status     string     `json:"status"`
	Available  bool       `json:"available"`
	UpdatedAt  *time.Time `json:"updated_at"`
	Message    string     `json:"message"`
	ServerTime time.Time  `json:"server_time"`
}

type balanceCache struct {
	Key       string
	FetchedAt time.Time
	ExpiresAt time.Time
	View      balanceView
}

func (a *App) adminBalance(w http.ResponseWriter, r *http.Request) {
	// Keep the account fixed through this bounded read. Settings changes use the
	// write side, so a response can never label one account's balance as another.
	a.providerGate.RLock()
	defer a.providerGate.RUnlock()
	view := balanceView{Currency: "USD", Mode: a.opts.Mode, Status: "unconfigured", Message: "尚未配置资源平台 API 密钥"}
	client := a.currentClient()
	if client == nil {
		a.balanceResponse(w, view)
		return
	}
	balanceClient, ok := client.(provider.BalanceClient)
	if !ok {
		view.Status, view.Message = "unavailable", "暂时无法读取上游余额"
		a.balanceResponse(w, view)
		return
	}
	key := hash(a.opts.Mode + "\x00" + a.opts.APIBase + "\x00" + a.currentAPIKey())
	// Coalesce concurrent callers, including forced refreshes. A completed
	// lookup is reused for at least two seconds; failures for five seconds.
	a.balanceMu.Lock()
	defer a.balanceMu.Unlock()
	now := time.Now()
	cached := a.balanceCache
	force := r.URL.Query().Get("refresh") == "1"
	if cached.Key == key && now.Before(cached.ExpiresAt) && (!force || !cached.View.Available || now.Before(cached.FetchedAt.Add(balanceRefreshInterval))) {
		a.balanceResponse(w, cached.View)
		return
	}
	// A browser closing its request must not invalidate the shared result for
	// other administrators. App shutdown and the timeout still cancel the read.
	ctx, cancel := context.WithTimeout(a.ctx, balanceTimeout)
	result, err := balanceClient.Balance(ctx)
	cancel()
	ttl := balanceFailureTTL
	view.Status, view.Message = "unavailable", "暂时无法读取上游余额"
	if err == nil && result.Valid() {
		updated := time.Now().UTC()
		view.Balance, view.UpdatedAt, view.Available = &result.Amount, &updated, true
		view.Status, view.Message = "available", "上游账户余额"
		if a.opts.Mode == "demo" {
			view.Status, view.Message = "demo", "演示余额（模拟）"
		}
		ttl = balanceSuccessTTL
	}
	// Never relay raw provider errors: they may contain credentials or URLs.
	fetchedAt := time.Now()
	a.balanceCache = balanceCache{Key: key, FetchedAt: fetchedAt, ExpiresAt: fetchedAt.Add(ttl), View: view}
	a.balanceResponse(w, view)
}

func (a *App) balanceResponse(w http.ResponseWriter, view balanceView) {
	view.ServerTime = time.Now().UTC()
	respond(w, http.StatusOK, view)
}
