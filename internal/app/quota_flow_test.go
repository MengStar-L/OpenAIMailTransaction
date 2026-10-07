package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"openai-mail-transaction/internal/provider"
)

type quotaProvider struct {
	*testProvider
	nextCalls int
	nextError error
}

func (p *quotaProvider) NextCode(context.Context, string, string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextCalls++
	return p.nextError
}

func (p *quotaProvider) setCode(code string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pollResult = provider.Result{Status: "waiting"}
	if code != "" {
		p.pollResult = provider.Result{Status: "received", Code: code}
	}
}

func (p *quotaProvider) nextCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nextCalls
}

type quotaHarness struct {
	app    *App
	opts   Options
	p      *quotaProvider
	code   string
	cookie *http.Cookie
}

func newQuotaHarness(t *testing.T, limit int) *quotaHarness {
	t.Helper()
	p := &quotaProvider{testProvider: &testProvider{}}
	f := &quotaHarness{p: p, opts: testOptions(t.TempDir(), p.testProvider)}
	f.opts.Client = p
	var err error
	f.app, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if f.app != nil {
			if err := f.app.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	setupTestAdmin(t, f.app, testAdminPassword)
	f.cookie = adminCookie(t, f.app)
	w := apiRequest(f.app.Handler(), http.MethodPost, "/api/admin/cdks", map[string]any{
		"kind": "email", "quantity": 1, "expires_days": 7, "usage_limit": limit,
	}, "", f.cookie, "http://example.test")
	if w.Code != http.StatusOK {
		t.Fatalf("issue quota voucher HTTP %d: %s", w.Code, w.Body.String())
	}
	var issued struct {
		Codes []string `json:"codes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &issued); err != nil || len(issued.Codes) != 1 {
		t.Fatalf("issued voucher response: %s (%v)", w.Body.String(), err)
	}
	f.code = issued.Codes[0]
	return f
}

func (f *quotaHarness) restart(t *testing.T) {
	t.Helper()
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	f.app = nil
	var err error
	f.app, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
}

func quotaPoll(t *testing.T, a *App, session orderEnvelope) orderEnvelope {
	t.Helper()
	// Advance only the local poll throttle; no wall-clock delay is needed to
	// simulate a provider delivering its next verification message.
	if _, err := a.db.Exec("UPDATE orders SET last_poll=0 WHERE id=?", session.Order.ID); err != nil {
		t.Fatal(err)
	}
	current := parseOrder(t, apiRequest(a.Handler(), http.MethodGet, "/api/orders/current", nil, session.Token, nil, ""))
	current.Token = session.Token
	return current
}

func quotaNext(t *testing.T, a *App, session orderEnvelope, round int) orderEnvelope {
	t.Helper()
	current := parseOrder(t, apiRequest(a.Handler(), http.MethodPost, "/api/orders/next-code", map[string]int{"round": round}, session.Token, nil, ""))
	current.Token = session.Token
	return current
}

func quotaAssertUsage(t *testing.T, a *App, session orderEnvelope, used, limit int) {
	t.Helper()
	o, err := a.getOrder(session.Order.ID)
	if err != nil {
		t.Fatal(err)
	}
	c, err := a.getCDK(o.CDKID)
	if err != nil {
		t.Fatal(err)
	}
	if c.UsedCount != used || c.UsageLimit != limit || session.Order.UsedCount != used || session.Order.UsageLimit != limit {
		t.Fatalf("quota DB=%d/%d response=%d/%d, want %d/%d", c.UsedCount, c.UsageLimit, session.Order.UsedCount, session.Order.UsageLimit, used, limit)
	}
}

func TestQuotaThreeEffectiveMailboxesEachKeepThreeReceiptRounds(t *testing.T) {
	f := newQuotaHarness(t, 3)
	session := redeem(t, f.app, f.code)
	quotaAssertUsage(t, f.app, session, 0, 3)
	for mailbox := 1; mailbox <= 3; mailbox++ {
		firstCode := fmt.Sprintf("%d00123", mailbox)
		f.p.setCode(firstCode)
		session = quotaPoll(t, f.app, session)
		if session.Order.Status != "received" || len(session.Order.Codes) != 1 || session.Order.MailRound != 1 || !session.Order.CanNextCode {
			t.Fatalf("mailbox %d first receipt: %+v", mailbox, session.Order)
		}
		quotaAssertUsage(t, f.app, session, mailbox, 3)
		// Polling the first result repeatedly is not another effective mailbox.
		session = quotaPoll(t, f.app, session)
		quotaAssertUsage(t, f.app, session, mailbox, 3)
		for round := 2; round <= 3; round++ {
			previousCode := session.Order.Code
			before := f.p.nextCount()
			session = quotaNext(t, f.app, session, round-1)
			if session.Order.Status != "waiting" || session.Order.MailRound != round || session.Order.Code != "" || len(session.Order.Codes) != round-1 {
				t.Fatalf("mailbox %d waiting round %d: %+v", mailbox, round, session.Order)
			}
			// Replaying the old button click must not advance to another round.
			session = quotaNext(t, f.app, session, round-1)
			if f.p.nextCount() != before+1 {
				t.Fatal("duplicate next-code action repeated the upstream transition")
			}
			f.p.setCode(previousCode)
			session = quotaPoll(t, f.app, session)
			if session.Order.Status != "waiting" || session.Order.Code != "" || len(session.Order.Codes) != round-1 {
				t.Fatalf("historical code was counted as a new receipt: %+v", session.Order)
			}
			quotaAssertUsage(t, f.app, session, mailbox, 3)
			f.p.setCode(fmt.Sprintf("%d00%d56", mailbox, round))
			session = quotaPoll(t, f.app, session)
			if session.Order.Status != "received" || len(session.Order.Codes) != round || session.Order.Codes[round-1].Round != round {
				t.Fatalf("mailbox %d received round %d: %+v", mailbox, round, session.Order)
			}
			quotaAssertUsage(t, f.app, session, mailbox, 3)
		}
		before := f.p.nextCount()
		w := apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/next-code", map[string]int{"round": 3}, session.Token, nil, "")
		if w.Code != http.StatusConflict || f.p.nextCount() != before || session.Order.CanNextCode {
			t.Fatalf("fourth receipt permitted: HTTP %d", w.Code)
		}
		completed := parseOrder(t, apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/complete", map[string]any{}, session.Token, nil, ""))
		if completed.Order.Status != "completed" || completed.Order.CanRetry != (mailbox < 3) {
			t.Fatalf("completion did not expose remaining mailbox quota: %+v", completed.Order)
		}
		quotaAssertUsage(t, f.app, completed, mailbox, 3)
		f.p.setCode("")
		w = apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/replace", map[string]any{}, session.Token, nil, "")
		if mailbox == 3 {
			if w.Code == http.StatusOK {
				if final := parseOrder(t, w); final.Order.ID != session.Order.ID || final.Order.CanRetry {
					t.Fatal("exhausted CDK returned a new or repeatable mailbox")
				}
			} else if w.Code != http.StatusConflict {
				t.Fatalf("exhausted CDK purchased another mailbox: HTTP %d", w.Code)
			}
		} else {
			replacement := parseOrder(t, w)
			if replacement.Order.ID == session.Order.ID || replacement.Token == "" || replacement.Order.Status != "waiting" {
				t.Fatalf("missing next effective mailbox: %+v", replacement)
			}
			session = replacement
		}
	}
	if allocations, _, _, completions := f.p.counts(); allocations != 3 || completions != 3 || f.p.nextCount() != 6 {
		t.Fatalf("expected 3 purchases, 3 closes, 6 continuations: got %d/%d/%d", allocations, completions, f.p.nextCount())
	}
}

func TestQuotaCancelledMailboxesDoNotSpendQuotaAndConcurrentReplaceBuysOnce(t *testing.T) {
	f := newQuotaHarness(t, 2)
	session := redeem(t, f.app, f.code)
	for i := 0; i < 5; i++ {
		cancelled := parseOrder(t, apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/cancel", map[string]any{}, session.Token, nil, ""))
		if cancelled.Order.Status != "cancelled" || !cancelled.Order.CanRetry {
			t.Fatalf("cancellation %d consumed quota or disabled retry: %+v", i, cancelled.Order)
		}
		quotaAssertUsage(t, f.app, cancelled, 0, 2)
		session = parseOrder(t, apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/replace", map[string]any{}, session.Token, nil, ""))
	}
	// This voucher has exceeded its legacy max_attempts=2 five times without
	// receiving a code, and must still be eligible for a real usable mailbox.
	cancelled := parseOrder(t, apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/cancel", map[string]any{}, session.Token, nil, ""))
	quotaAssertUsage(t, f.app, cancelled, 0, 2)
	const clients = 8
	responses := make(chan *httptest.ResponseRecorder, clients)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			responses <- apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/replace", map[string]any{}, session.Token, nil, "")
		}()
	}
	close(start)
	wg.Wait()
	close(responses)
	var replacement orderEnvelope
	for w := range responses {
		got := parseOrder(t, w)
		if replacement.Token == "" {
			replacement = got
		}
		if got.Order.ID != replacement.Order.ID || got.Token != replacement.Token {
			t.Fatal("old-token retries allocated different replacements")
		}
	}
	if allocations, _, cancels, _ := f.p.counts(); allocations != 7 || cancels != 6 {
		t.Fatalf("replacement calls=%d cancellations=%d, expected 7/6", allocations, cancels)
	}
	f.p.setCode("654321")
	replacement = quotaPoll(t, f.app, replacement)
	quotaAssertUsage(t, f.app, replacement, 1, 2)
}

func TestQuotaReplacementAndNextCodeRequireOrderAuthorization(t *testing.T) {
	f := newQuotaHarness(t, 3)
	session := redeem(t, f.app, f.code)
	for _, path := range []string{"/api/orders/replace", "/api/orders/next-code"} {
		for _, token := range []string{"", session.Token + "x"} {
			w := apiRequest(f.app.Handler(), http.MethodPost, path, map[string]any{"round": 1}, token, nil, "")
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("unauthorized %s HTTP %d", path, w.Code)
			}
		}
	}
	w := apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/replace", map[string]any{}, session.Token, nil, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("waiting mailbox replaced: HTTP %d", w.Code)
	}
	w = apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/next-code", map[string]int{"round": 1}, session.Token, nil, "https://attacker.test")
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin next-code HTTP %d", w.Code)
	}
	if allocations, _, _, _ := f.p.counts(); allocations != 1 || f.p.nextCount() != 0 {
		t.Fatal("unauthorized or active replacement reached upstream")
	}
}

func TestQuotaConcurrentNextCodeRequestsAdvanceOnlyOneRound(t *testing.T) {
	f := newQuotaHarness(t, 1)
	session := redeem(t, f.app, f.code)
	f.p.setCode("001234")
	session = quotaPoll(t, f.app, session)
	const clients = 8
	responses := make(chan *httptest.ResponseRecorder, clients)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			responses <- apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/next-code", map[string]int{"round": 1}, session.Token, nil, "")
		}()
	}
	close(start)
	wg.Wait()
	close(responses)
	for w := range responses {
		got := parseOrder(t, w)
		if got.Order.ID != session.Order.ID || got.Order.MailRound != 2 || got.Order.Status != "waiting" || len(got.Order.Codes) != 1 {
			t.Fatalf("concurrent continuation returned inconsistent receipt round: %+v", got.Order)
		}
		quotaAssertUsage(t, f.app, got, 1, 1)
	}
	if f.p.nextCount() != 1 {
		t.Fatalf("concurrent continuation sent status=5 %d times", f.p.nextCount())
	}
}

func TestQuotaRejectedNextCodeKeepsReceivedRoundAndHistory(t *testing.T) {
	f := newQuotaHarness(t, 1)
	session := redeem(t, f.app, f.code)
	f.p.setCode("001234")
	session = quotaPoll(t, f.app, session)
	f.p.mu.Lock()
	f.p.nextError = &provider.Error{Code: "bad_status", Message: "平台拒绝当前状态"}
	f.p.mu.Unlock()
	session = quotaNext(t, f.app, session, 1)
	if session.Order.Status != "received" || session.Order.MailRound != 1 || session.Order.Code != "001234" || len(session.Order.Codes) != 1 || !session.Order.CanNextCode {
		t.Fatalf("explicit rejection consumed a mail round: %+v", session.Order)
	}
	quotaAssertUsage(t, f.app, session, 1, 1)
	f.p.mu.Lock()
	f.p.nextError = nil
	f.p.mu.Unlock()
	session = quotaNext(t, f.app, session, 1)
	if session.Order.Status != "waiting" || session.Order.MailRound != 2 || f.p.nextCount() != 2 {
		t.Fatalf("next successful attempt skipped a receipt round: %+v", session.Order)
	}
}

func TestQuotaSameCodeNeedsNewWaitingEvidenceThatSurvivesRestart(t *testing.T) {
	f := newQuotaHarness(t, 1)
	session := redeem(t, f.app, f.code)
	f.p.setCode("001234")
	session = quotaPoll(t, f.app, session)
	session = quotaNext(t, f.app, session, 1)
	session = quotaPoll(t, f.app, session)
	if session.Order.Status != "waiting" || len(session.Order.Codes) != 1 {
		t.Fatal("unchanged upstream code was mistaken for a new delivery")
	}
	// The upstream acknowledges an actual wait, and later delivers an equal
	// code string. Verification codes are not unique message identifiers.
	f.p.setCode("")
	session = quotaPoll(t, f.app, session)
	if session.Order.Status != "waiting" {
		t.Fatalf("waiting evidence changed order state: %+v", session.Order)
	}
	f.restart(t)
	f.p.setCode("001234")
	session = quotaPoll(t, f.app, session)
	if session.Order.Status != "received" || session.Order.MailRound != 2 || len(session.Order.Codes) != 2 || session.Order.Codes[0].Code != session.Order.Codes[1].Code {
		t.Fatalf("equal new code lost its persisted waiting evidence: %+v", session.Order)
	}
	quotaAssertUsage(t, f.app, session, 1, 1)
}

func TestQuotaUncertainNextRoundSurvivesRestartWithoutReplayingTransition(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprintf("interrupted_%t", interrupted), func(t *testing.T) {
			f := newQuotaHarness(t, 1)
			session := redeem(t, f.app, f.code)
			f.p.setCode("100001")
			session = quotaPoll(t, f.app, session)
			f.p.mu.Lock()
			f.p.nextError = &provider.Error{Code: "timeout", Message: "结果待核对", Uncertain: true}
			f.p.mu.Unlock()
			session = quotaNext(t, f.app, session, 1)
			if session.Order.Status != "next_uncertain" || session.Order.MailRound != 2 || len(session.Order.Codes) != 1 {
				t.Fatalf("uncertain next-code result lost intent or first code: %+v", session.Order)
			}
			if interrupted {
				// Reproduce a crash after persisting intent but before saving the
				// provider response. Recovery must keep the request uncertain.
				if _, err := f.app.db.Exec("UPDATE orders SET status='next_pending' WHERE id=?", session.Order.ID); err != nil {
					t.Fatal(err)
				}
			}
			f.restart(t)
			session = quotaPoll(t, f.app, session)
			if session.Order.Status != "next_uncertain" || len(session.Order.Codes) != 1 {
				t.Fatalf("restart treated an old code as a new receipt: %+v", session.Order)
			}
			session = quotaNext(t, f.app, session, 1)
			if f.p.nextCount() != 1 {
				t.Fatal("restart or repeated click replayed uncertain status=5")
			}
			w := apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/replace", map[string]any{}, session.Token, nil, "")
			if w.Code != http.StatusConflict {
				t.Fatalf("uncertain mailbox replaced: HTTP %d", w.Code)
			}
			f.p.setCode("200002")
			session = quotaPoll(t, f.app, session)
			if session.Order.Status != "received" || session.Order.MailRound != 2 || len(session.Order.Codes) != 2 || !session.Order.CanNextCode {
				t.Fatalf("new code did not confirm second receipt: %+v", session.Order)
			}
			quotaAssertUsage(t, f.app, session, 1, 1)
			if allocations, _, _, _ := f.p.counts(); allocations != 1 || f.p.nextCount() != 1 {
				t.Fatal("reconciliation made a paid replacement or repeated continuation")
			}
		})
	}
}

func TestQuotaCompletionPendingBlocksNewPurchaseAndKeepsReceivedCode(t *testing.T) {
	f := newQuotaHarness(t, 3)
	session := redeem(t, f.app, f.code)
	f.p.setCode("012349")
	session = quotaPoll(t, f.app, session)
	f.p.mu.Lock()
	f.p.completeError = &provider.Error{Code: "timeout", Message: "完成待确认", Uncertain: true}
	f.p.mu.Unlock()
	completed := parseOrder(t, apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/complete", map[string]any{}, session.Token, nil, ""))
	if completed.Order.Status != "complete_pending" || completed.Order.Code != "012349" || completed.Order.CanRetry {
		t.Fatalf("unconfirmed completion discarded reservation: %+v", completed.Order)
	}
	quotaAssertUsage(t, f.app, completed, 1, 3)
	f.restart(t)
	w := apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/replace", map[string]any{}, session.Token, nil, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("unfinished completion permitted replacement: HTTP %d", w.Code)
	}
	resumed := redeem(t, f.app, f.code)
	if resumed.Order.ID != session.Order.ID || resumed.Order.Code != "012349" {
		t.Fatal("CDK resume lost the unresolved mailbox")
	}
	if allocations, _, _, _ := f.p.counts(); allocations != 1 {
		t.Fatal("pending completion allowed another paid resource")
	}
	f.p.mu.Lock()
	f.p.completeError = nil
	f.p.mu.Unlock()
	advanceCompletionRetry(t, f.app, session.Order.ID)
	completed = parseOrder(t, apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/complete", map[string]any{}, session.Token, nil, ""))
	if completed.Order.Status != "completed" || !completed.Order.CanRetry {
		t.Fatalf("confirmed completion did not enable remaining quota: %+v", completed.Order)
	}
	replacement := parseOrder(t, apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/replace", map[string]any{}, session.Token, nil, ""))
	if replacement.Order.ID == session.Order.ID {
		t.Fatal("confirmed completion failed to advance to the next mailbox")
	}
	quotaAssertUsage(t, f.app, replacement, 1, 3)
}

func TestQuotaFullVoucherVisibleOnlyToAuthenticatedAdmin(t *testing.T) {
	f := newQuotaHarness(t, 3)
	session := redeem(t, f.app, f.code)
	path := "/api/admin/cdks?query=" + url.QueryEscape(f.code)
	list := apiRequest(f.app.Handler(), http.MethodGet, path, nil, "", f.cookie, "")
	if list.Code != http.StatusOK {
		t.Fatalf("admin full-code search HTTP %d: %s", list.Code, list.Body.String())
	}
	var result struct {
		Items []CDK `json:"items"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &result); err != nil || len(result.Items) != 1 || result.Items[0].Code != f.code {
		t.Fatalf("admin could not retrieve complete CDK: %s (%v)", list.Body.String(), err)
	}
	if bytes.Contains(list.Body.Bytes(), []byte(hash(f.code))) || bytes.Contains(list.Body.Bytes(), []byte("code_cipher")) {
		t.Fatal("admin JSON leaked storage encryption or voucher hash")
	}
	overview := apiRequest(f.app.Handler(), http.MethodGet, "/api/admin/overview", nil, "", f.cookie, "")
	if overview.Code != http.StatusOK || !bytes.Contains(overview.Body.Bytes(), []byte(f.code)) {
		t.Fatal("admin order overview does not show its complete CDK")
	}
	for _, endpoint := range []string{path, "/api/admin/overview"} {
		w := apiRequest(f.app.Handler(), http.MethodGet, endpoint, nil, "", nil, "")
		if w.Code != http.StatusUnauthorized || bytes.Contains(w.Body.Bytes(), []byte(f.code)) {
			t.Fatalf("unauthed administrator endpoint exposed a voucher: HTTP %d", w.Code)
		}
	}
	for _, endpoint := range []string{"/api/config", "/api/orders/current"} {
		w := apiRequest(f.app.Handler(), http.MethodGet, endpoint, nil, session.Token, nil, "")
		if w.Code != http.StatusOK {
			t.Fatalf("public %s HTTP %d", endpoint, w.Code)
		}
		for _, secret := range []string{f.code, hash(f.code), "code_cipher", "provider_id", "cdk_code"} {
			if bytes.Contains(w.Body.Bytes(), []byte(secret)) {
				t.Fatalf("public %s exposed %s", endpoint, secret)
			}
		}
	}
}
