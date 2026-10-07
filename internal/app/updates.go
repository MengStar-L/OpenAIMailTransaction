package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"openai-mail-transaction/internal/updater"
)

// A restart must not interrupt a paid provider action or an active mailbox.
func (a *App) CanUpdate() error {
	a.providerGate.Lock()
	defer a.providerGate.Unlock()
	if err := a.checkUpdateOrders(); err != nil {
		return err
	}
	a.updateMaintenance.Store(true)
	return nil
}

func (a *App) CancelUpdate() { a.updateMaintenance.Store(false) }

func (a *App) checkUpdateOrders() error {
	var count int
	err := a.db.QueryRow("SELECT count(*) FROM orders WHERE status IN ('queued','allocating','waiting','received','next_pending','next_uncertain','complete_pending','cancel_pending')").Scan(&count)
	if err != nil {
		return errors.New("无法确认订单状态，暂缓更新")
	}
	if count > 0 {
		return errors.New("仍有进行中的订单，结束后再更新")
	}
	return nil
}

func (a *App) updateStatus(w http.ResponseWriter, r *http.Request) {
	if a.opts.Updates == nil {
		fail(w, http.StatusServiceUnavailable, "当前运行方式未启用更新")
		return
	}
	respond(w, http.StatusOK, a.opts.Updates.State())
}

func (a *App) updatePreferences(w http.ResponseWriter, r *http.Request) {
	if a.opts.Updates == nil {
		fail(w, http.StatusServiceUnavailable, "当前运行方式未启用更新")
		return
	}
	var settings updater.Settings
	if !decode(w, r, &settings) {
		return
	}
	if err := a.opts.Updates.SetSettings(settings); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	respond(w, http.StatusOK, a.opts.Updates.State())
}

func (a *App) checkUpdate(w http.ResponseWriter, r *http.Request) {
	if a.opts.Updates == nil {
		fail(w, http.StatusServiceUnavailable, "当前运行方式未启用更新")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	state, err := a.opts.Updates.Check(ctx)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, updater.ErrBusy) {
			code = http.StatusConflict
		}
		fail(w, code, err.Error())
		return
	}
	respond(w, http.StatusOK, state)
}

func (a *App) applyUpdate(w http.ResponseWriter, r *http.Request) {
	if a.opts.Updates == nil {
		fail(w, http.StatusServiceUnavailable, "当前运行方式未启用更新")
		return
	}
	if err := a.checkUpdateOrders(); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	if !a.updateApplying.CompareAndSwap(false, true) {
		fail(w, http.StatusConflict, "正在安装更新")
		return
	}
	state := a.opts.Updates.State()
	if !state.Supported || !state.Available {
		a.updateApplying.Store(false)
		fail(w, http.StatusConflict, "没有可安装的更新")
		return
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer a.updateApplying.Store(false)
		ctx, cancel := context.WithTimeout(a.ctx, 8*time.Minute)
		defer cancel()
		_, _ = a.opts.Updates.Apply(ctx)
	}()
	state.Phase = "downloading"
	state.Error = ""
	state.DownloadedBytes = 0
	state.TotalBytes = 0
	respond(w, http.StatusAccepted, state)
}
