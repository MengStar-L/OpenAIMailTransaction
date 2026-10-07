package app

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"openai-mail-transaction/internal/provider"
)

func automaticStep(t *testing.T, f *quotaHarness, id string) Order {
	t.Helper()
	if _, err := f.app.db.Exec("UPDATE orders SET last_poll=0 WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	if err := f.app.processActiveOrder(id); err != nil {
		t.Fatal(err)
	}
	o, err := f.app.getOrder(id)
	if err != nil {
		t.Fatal(err)
	}
	f.app.decorate(&o)
	return o
}

func TestAutomaticMailReceivesThreeRoundsWithoutBrowserActions(t *testing.T) {
	f := newQuotaHarness(t, 3)
	session := redeem(t, f.app, f.code)
	for round := 1; round <= 3; round++ {
		code := fmt.Sprintf("%06d", round)
		f.p.setCode(code)
		o := automaticStep(t, f, session.Order.ID)
		if len(o.Codes) != round || o.Codes[round-1].Code != code || o.UsedCount != 1 {
			t.Fatalf("automatic receipt/accounting round %d: %+v", round, o)
		}
		if round < 3 {
			if o.Status != "waiting" || o.MailRound != round+1 || o.Code != "" || f.p.nextCount() != round {
				t.Fatalf("round %d did not continue automatically: %+v", round, o)
			}
			// The old provider response can survive status=5; neither timer
			// replay nor a stale browser button may count it or send again.
			repeated := automaticStep(t, f, o.ID)
			if len(repeated.Codes) != round || repeated.MailRound != round+1 {
				t.Fatal("repeated provider response advanced the mailbox")
			}
			quotaNext(t, f.app, session, round)
			if f.p.nextCount() != round {
				t.Fatal("old manual action duplicated automatic continuation")
			}
		} else if o.Status != "received" || o.MailRound != 3 || o.CanNextCode || o.AutoNextState != "" {
			t.Fatalf("third mail should remain available for completion: %+v", o)
		}
	}
	if calls, _, _, closes := f.p.counts(); calls != 1 || closes != 0 || f.p.nextCount() != 2 {
		t.Fatalf("automatic flow purchased/closed/repeated a mailbox: %d/%d/%d", calls, closes, f.p.nextCount())
	}
}

func TestAutomaticMailDefiniteRejectionsHaveDurableBudgetAndManualRecovery(t *testing.T) {
	f := newQuotaHarness(t, 1)
	session := redeem(t, f.app, f.code)
	f.p.setCode("001234")
	f.p.nextError = &provider.Error{Code: "bad_status", Message: "尚不能继续"}
	for attempt := 1; attempt <= autoMailMaxAttempts; attempt++ {
		if attempt > 1 {
			if _, err := f.app.db.Exec("UPDATE order_auto_mail SET next_attempt_at=0 WHERE order_id=?", session.Order.ID); err != nil {
				t.Fatal(err)
			}
		}
		o := automaticStep(t, f, session.Order.ID)
		wantState := "retrying"
		if attempt == autoMailMaxAttempts {
			wantState = "failed"
		}
		if o.Status != "received" || o.MailRound != 1 || o.Code != "001234" || len(o.Codes) != 1 || o.AutoNextState != wantState || f.p.nextCount() != attempt {
			t.Fatalf("attempt %d lost receipt or retry limit: %+v", attempt, o)
		}
		f.restart(t)
		automaticStep(t, f, session.Order.ID)
		if f.p.nextCount() != attempt {
			t.Fatal("restart lost automatic retry delay/budget")
		}
	}
	f.p.nextError = nil
	resumed := quotaNext(t, f.app, session, 1)
	if resumed.Order.Status != "waiting" || resumed.Order.MailRound != 2 || f.p.nextCount() != 4 {
		t.Fatal("manual recovery after exhausted automatic attempts failed")
	}
	f.p.setCode("002345")
	o := automaticStep(t, f, session.Order.ID)
	if o.Status != "waiting" || o.MailRound != 3 || f.p.nextCount() != 5 || o.UsedCount != 1 {
		t.Fatalf("new round did not receive its own retry budget: %+v", o)
	}
}

func TestAutomaticMailUncertainContinuationNeverReplayedAfterRestart(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending_%t", interrupted), func(t *testing.T) {
			f := newQuotaHarness(t, 1)
			session := redeem(t, f.app, f.code)
			f.p.setCode("100001")
			f.p.nextError = &provider.Error{Code: "timeout", Message: "结果待确认", Uncertain: true}
			o := automaticStep(t, f, session.Order.ID)
			if o.Status != "next_uncertain" || len(o.Codes) != 1 || o.MailRound != 2 {
				t.Fatalf("uncertain automatic continuation lost durable intent: %+v", o)
			}
			if interrupted {
				if _, err := f.app.db.Exec("UPDATE orders SET status='next_pending' WHERE id=?", o.ID); err != nil {
					t.Fatal(err)
				}
			}
			f.restart(t)
			for range 3 {
				o = automaticStep(t, f, o.ID)
			}
			if f.p.nextCount() != 1 || o.Status != "next_uncertain" || len(o.Codes) != 1 {
				t.Fatal("uncertain mutation was replayed or an old code was consumed")
			}
			// A different real code proves that round two did start. Only
			// then may the worker start the distinct third-round transition.
			f.p.nextError = nil
			f.p.setCode("200002")
			o = automaticStep(t, f, o.ID)
			if o.Status != "waiting" || o.MailRound != 3 || len(o.Codes) != 2 || f.p.nextCount() != 2 || o.UsedCount != 1 {
				t.Fatalf("confirmed second mail did not permit the third: %+v", o)
			}
		})
	}
}

func TestAutomaticAndManualContinuationShareTheSameLock(t *testing.T) {
	f := newQuotaHarness(t, 1)
	session := redeem(t, f.app, f.code)
	f.p.setCode("123456")
	session = quotaPoll(t, f.app, session)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				results <- f.app.processActiveOrder(session.Order.ID)
			} else {
				w := apiRequest(f.app.Handler(), http.MethodPost, "/api/orders/next-code", map[string]int{"round": 1}, session.Token, nil, "")
				if w.Code != http.StatusOK {
					results <- fmt.Errorf("manual request HTTP %d", w.Code)
				} else {
					results <- nil
				}
			}
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	o, _ := f.app.getOrder(session.Order.ID)
	if f.p.nextCount() != 1 || o.MailRound != 2 || len(o.Codes) != 1 {
		t.Fatal("manual/automatic race repeated the upstream continuation")
	}
}

func TestAutomaticMailAcknowledgmentSaveFailureDoesNotReplay(t *testing.T) {
	f := newQuotaHarness(t, 1)
	session := redeem(t, f.app, f.code)
	f.p.setCode("123456")
	_, err := f.app.db.Exec(`CREATE TRIGGER reject_auto_ack BEFORE UPDATE ON orders
 WHEN OLD.status='next_pending' AND NEW.status='waiting'
 BEGIN SELECT RAISE(ABORT,'test acknowledgment save failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.app.processActiveOrder(session.Order.ID); err == nil {
		t.Fatal("acknowledgment fault unexpectedly saved")
	}
	o, _ := f.app.getOrder(session.Order.ID)
	if o.Status != "next_pending" || o.MailRound != 2 || len(o.Codes) != 1 || !o.UsageCounted || f.p.nextCount() != 1 {
		t.Fatalf("acknowledgment failure lost durable intent/history: %+v", o)
	}
	if _, err = f.app.db.Exec("DROP TRIGGER reject_auto_ack"); err != nil {
		t.Fatal(err)
	}
	o = automaticStep(t, f, o.ID)
	if o.Status != "next_uncertain" || f.p.nextCount() != 1 {
		t.Fatal("lost acknowledgment caused duplicate automatic continuation")
	}
	f.p.setCode("654321")
	o = automaticStep(t, f, o.ID)
	if o.Status != "waiting" || o.MailRound != 3 || len(o.Codes) != 2 || o.UsedCount != 1 || f.p.nextCount() != 2 {
		t.Fatalf("receipt reconciliation failed after storage recovered: %+v", o)
	}
}

func TestAutomaticMailAlsoContinuesAdministratorResources(t *testing.T) {
	f := newQuotaHarness(t, 1)
	session := adminAllocate(t, f.app, f.cookie, "email", "automatic-admin-receipt-001")
	f.p.setCode("123456")
	o := automaticStep(t, f, session.Order.ID)
	if o.Status != "waiting" || o.MailRound != 2 || len(o.Codes) != 1 || o.CDKID != "" || o.Source != "admin" || o.UsedCount != 0 || f.p.nextCount() != 1 {
		t.Fatalf("administrator mailbox did not continue without voucher use: %+v", o)
	}
	var spent int
	if err := f.app.db.QueryRow("SELECT SUM(used_count) FROM cdks").Scan(&spent); err != nil || spent != 0 {
		t.Fatalf("administrator continuation consumed a voucher: %d (%v)", spent, err)
	}
}

func TestAutomaticMailWorkerResumesPersistedReceiptWithoutOpenPage(t *testing.T) {
	f := newQuotaHarness(t, 1)
	session := redeem(t, f.app, f.code)
	f.p.setCode("123456")
	session = quotaPoll(t, f.app, session)
	f.opts.DisableWorker = false
	f.restart(t)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		o, err := f.app.getOrder(session.Order.ID)
		if err != nil {
			t.Fatal(err)
		}
		if o.Status == "waiting" && o.MailRound == 2 {
			if f.p.nextCount() != 1 || len(o.Codes) != 1 {
				t.Fatal("timer lost history or repeated the continuation")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("background worker did not resume received mail after restart")
}

func TestAutomaticMailNeverContinuesAfterDeadlineOrDisabledVoucher(t *testing.T) {
	for _, reason := range []string{"expired", "disabled"} {
		t.Run(reason, func(t *testing.T) {
			f := newQuotaHarness(t, 1)
			session := redeem(t, f.app, f.code)
			f.p.setCode("123456")
			session = quotaPoll(t, f.app, session)
			if reason == "expired" {
				_, _ = f.app.db.Exec("UPDATE orders SET expires_at=1 WHERE id=?", session.Order.ID)
			} else {
				_, _ = f.app.db.Exec("UPDATE cdks SET status='disabled'")
			}
			automaticStep(t, f, session.Order.ID)
			if f.p.nextCount() != 0 {
				t.Fatal("expired or disabled mailbox requested another mail")
			}
		})
	}
}

func TestEmailTwentyFiveMinuteMigrationIsScopedAndRunsOnlyOnce(t *testing.T) {
	f := newQuotaHarness(t, 1)
	session := redeem(t, f.app, f.code)
	_, err := f.app.db.Exec(`UPDATE settings SET value=json_set(value,'$.email_ttl_minutes',20);
UPDATE cdks SET snapshot=json_set(snapshot,'$.email_ttl_minutes',20);
UPDATE order_allocation_queue SET request_json=json_set(request_json,'$.TTL',1200000000000);
DELETE FROM metadata WHERE key='email_wait_25_minutes_v1';`)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := f.app.getOrder(session.Order.ID)
	if err = migrateAutomaticMail(f.app.db); err != nil {
		t.Fatal(err)
	}
	var setting, mailSnapshot, phoneSnapshot int
	_ = f.app.db.QueryRow("SELECT json_extract(value,'$.email_ttl_minutes') FROM settings").Scan(&setting)
	_ = f.app.db.QueryRow("SELECT json_extract(snapshot,'$.email_ttl_minutes') FROM cdks WHERE kind='email' LIMIT 1").Scan(&mailSnapshot)
	_ = f.app.db.QueryRow("SELECT json_extract(snapshot,'$.email_ttl_minutes') FROM cdks WHERE kind='phone' LIMIT 1").Scan(&phoneSnapshot)
	current, _ := f.app.getOrder(session.Order.ID)
	q, _ := f.app.getAllocationQueue(session.Order.ID)
	if setting != 25 || mailSnapshot != 25 || phoneSnapshot != 20 || !current.ExpiresAt.Equal(original.ExpiresAt) || q.Request.TTL != 20*time.Minute {
		t.Fatalf("migration exceeded scope or failed defaults: %d/%d/%d, deadline %s, request %s", setting, mailSnapshot, phoneSnapshot, current.ExpiresAt, q.Request.TTL)
	}
	// A queued purchase has no upstream deadline yet; its future activation
	// gets 25 minutes without extending the separate stock-queue deadline.
	_, err = f.app.db.Exec("UPDATE orders SET status='queued',provider_id='',resource='' WHERE id=?", session.Order.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.app.db.Exec("DELETE FROM metadata WHERE key='email_wait_25_minutes_v1'")
	if err = migrateAutomaticMail(f.app.db); err != nil {
		t.Fatal(err)
	}
	queued, _ := f.app.getAllocationQueue(session.Order.ID)
	if queued.Request.TTL != 25*time.Minute || !queued.ExpiresAt.Equal(q.ExpiresAt) {
		t.Fatal("queued resource lifetime was not migrated independently of queue time")
	}
	_, _ = f.app.db.Exec("UPDATE settings SET value=json_set(value,'$.email_ttl_minutes',20)")
	if err = migrateAutomaticMail(f.app.db); err != nil {
		t.Fatal(err)
	}
	_ = f.app.db.QueryRow("SELECT json_extract(value,'$.email_ttl_minutes') FROM settings").Scan(&setting)
	if setting != 20 {
		t.Fatal("a later deliberate administrator setting was overwritten on restart")
	}
}
