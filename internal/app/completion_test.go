package app

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"openai-mail-transaction/internal/provider"
)

type completionStatusProvider struct {
	*quotaProvider
	statusResult provider.Result
	statusError  error
	statusCalls  int
}

func (p *completionStatusProvider) CompletionStatus(context.Context, string, string) (provider.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.statusCalls++
	return p.statusResult, p.statusError
}

func advanceCompletionRetry(t *testing.T, a *App, id string) {
	t.Helper()
	if _, err := a.db.Exec("UPDATE order_completions SET next_attempt_at=0 WHERE order_id=?", id); err != nil {
		t.Fatal(err)
	}
}

func completionFixture(t *testing.T) (*quotaHarness, *completionStatusProvider, orderEnvelope) {
	t.Helper()
	f := newQuotaHarness(t, 3)
	p := &completionStatusProvider{quotaProvider: f.p, statusResult: provider.Result{Status: "waiting"}}
	f.app.client, f.opts.Client = p, p
	session := redeem(t, f.app, f.code)
	f.p.setCode("112233")
	session = quotaPoll(t, f.app, session)
	session = quotaNext(t, f.app, session, 1)
	f.p.setCode("445566")
	session = quotaPoll(t, f.app, session)
	return f, p, session
}

func TestPendingCompletionReconcilesConfirmedStatusWithoutReplay(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "durable-restart", true: "legacy-pending-upgrade"}[legacy], func(t *testing.T) {
			f, p, session := completionFixture(t)
			p.completeError = &provider.Error{Code: "upstream_unavailable", Message: "资源平台连接失败，请稍后查看订单"}
			pending := parseOrder(t, apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/complete", map[string]any{}, session.Token, nil, ""))
			if pending.Order.Status != "complete_pending" || !reflect.DeepEqual(pending.Order.Codes, session.Order.Codes) {
				t.Fatalf("completion intent lost receipts: %+v", pending.Order)
			}
			state, err := f.app.completionState(session.Order.ID)
			if err != nil || state.Attempts != 1 {
				t.Fatalf("completion attempt not durable: %+v %v", state, err)
			}
			if legacy {
				if _, err = f.app.db.Exec("DELETE FROM order_completions WHERE order_id=?", session.Order.ID); err != nil {
					t.Fatal(err)
				}
			}
			f.restart(t)
			p.statusResult = provider.Result{Status: "completed", Code: "not-a-new-receipt"}
			advanceCompletionRetry(t, f.app, session.Order.ID)
			completed := quotaPoll(t, f.app, session)
			if completed.Order.Status != "completed" || !completed.Order.CanRetry || !reflect.DeepEqual(completed.Order.Codes, session.Order.Codes) {
				t.Fatalf("confirmed status did not retain both receipts: %+v", completed.Order)
			}
			quotaAssertUsage(t, f.app, completed, 1, 3)
			_, _, _, completions := p.counts()
			if completions != 1 || p.statusCalls != 1 {
				t.Fatalf("replayed confirmed completion: mutations=%d lookups=%d", completions, p.statusCalls)
			}
			stored, err := f.app.completionState(session.Order.ID)
			if err != nil || !legacy && !stored.StartedAt.Equal(state.StartedAt) {
				t.Fatal("restart reset completion deadline")
			}
		})
	}
}

func TestCompletionRetriesAreBoundedAcrossRestartAndKeepQuota(t *testing.T) {
	f, p, session := completionFixture(t)
	p.completeError = &provider.Error{Code: "invalid_response", Message: "资源平台返回异常，请稍后查看订单或联系管理员"}
	pending := parseOrder(t, apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/complete", map[string]any{}, session.Token, nil, ""))
	if pending.Order.Status != "complete_pending" {
		t.Fatal("transient completion failure was not pending")
	}
	// Repeated clicks must respect the persisted retry interval.
	parseOrder(t, apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/complete", map[string]any{}, session.Token, nil, ""))
	if _, _, _, count := p.counts(); count != 1 {
		t.Fatal("repeated click ignored the retry interval")
	}
	advanceCompletionRetry(t, f.app, session.Order.ID)
	pending = quotaPoll(t, f.app, session)
	if pending.Order.Status != "complete_pending" || p.statusCalls != 1 {
		t.Fatal("nonterminal status was incorrectly accepted as completion")
	}
	started := time.Now().Add(-completionRetryWindow - time.Second).UTC().Truncate(time.Millisecond)
	if _, err := f.app.db.Exec("UPDATE order_completions SET started_at=? WHERE order_id=?", millis(started), session.Order.ID); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	completed := quotaPoll(t, f.app, session)
	if completed.Order.Status != "review" || completed.Order.CanRetry || !strings.Contains(completed.Order.Message, "超时") || !reflect.DeepEqual(completed.Order.Codes, session.Order.Codes) {
		t.Fatalf("confirmation exceeded its bounded window: %+v", completed.Order)
	}
	state, err := f.app.completionState(session.Order.ID)
	if err != nil || !state.StartedAt.Equal(started) {
		t.Fatal("restart extended confirmation deadline")
	}
	quotaAssertUsage(t, f.app, completed, 1, 3)
	if _, _, _, count := p.counts(); count != 2 {
		t.Fatal("expired deadline sent another completion mutation")
	}
	blocked := apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/replace", map[string]any{}, session.Token, nil, "")
	if blocked.Code != http.StatusConflict {
		t.Fatal("uncertain completion released a paid resource reservation")
	}
}

func TestDefinitiveCompletionRejectionReconcilesOrRequiresReview(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "still-active", true: "already-completed"}[terminal], func(t *testing.T) {
			f, p, session := completionFixture(t)
			p.completeError = &provider.Error{Code: "bad_status", Message: "资源平台订单状态已变化，请刷新或联系管理员"}
			if terminal {
				p.statusResult.Status = "completed"
			}
			completed := parseOrder(t, apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/complete", map[string]any{}, session.Token, nil, ""))
			want := "review"
			if terminal {
				want = "completed"
			}
			if completed.Order.Status != want || !reflect.DeepEqual(completed.Order.Codes, session.Order.Codes) {
				t.Fatalf("definitive rejection outcome: %+v", completed.Order)
			}
			quotaAssertUsage(t, f.app, completed, 1, 3)
			if _, _, _, count := p.counts(); count != 1 || p.statusCalls != 1 {
				t.Fatal("rejection was repeated instead of reconciled")
			}
			if !terminal && !strings.Contains(completed.Order.Message, "订单状态已变化") {
				t.Fatal("upstream rejection reason was hidden")
			}
		})
	}
}

func TestCompletionStatusOutageDoesNotReplayOrLoseReceipts(t *testing.T) {
	f, p, session := completionFixture(t)
	p.completeError = &provider.Error{Code: "upstream_unavailable", Message: "资源平台连接失败"}
	parseOrder(t, apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/complete", map[string]any{}, session.Token, nil, ""))
	p.statusError = &provider.Error{Code: "upstream_http", Message: "资源平台暂不可用", RetryAfter: time.Hour}
	advanceCompletionRetry(t, f.app, session.Order.ID)
	pending := quotaPoll(t, f.app, session)
	if pending.Order.Status != "complete_pending" || !strings.Contains(pending.Order.Message, "暂不可用") || !reflect.DeepEqual(pending.Order.Codes, session.Order.Codes) {
		t.Fatalf("status outage lost pending confirmation: %+v", pending.Order)
	}
	if _, _, _, count := p.counts(); count != 1 {
		t.Fatal("replayed completion while read-only reconciliation failed")
	}
	if _, err := f.app.db.Exec("UPDATE order_completions SET started_at=? WHERE order_id=?", time.Now().Add(-completionRetryWindow-time.Second).UnixMilli(), session.Order.ID); err != nil {
		t.Fatal(err)
	}
	review := quotaPoll(t, f.app, session)
	if review.Order.Status != "review" {
		t.Fatal("long Retry-After extended the two-minute confirmation bound")
	}
	quotaAssertUsage(t, f.app, review, 1, 3)
}
