package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"openai-mail-transaction/internal/provider"
)

type responseAfterStockWindow struct {
	*testProvider
	window     time.Duration
	firstStock bool
	resultErr  error
	deadlines  []time.Duration
}

func (p *responseAfterStockWindow) Allocate(ctx context.Context, req provider.Request) (provider.Activation, error) {
	p.allocations++
	p.requests = append(p.requests, req)
	deadline, _ := ctx.Deadline()
	p.deadlines = append(p.deadlines, time.Until(deadline))
	if p.firstStock && p.allocations == 1 {
		return provider.Activation{}, phoneStockError()
	}
	// The response arrives after the stock window whether this is the first
	// request or a retry, but well inside the individual request deadline.
	timer := time.NewTimer(p.window + 15*time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return provider.Activation{}, &provider.Error{Code: "upstream_unavailable", Uncertain: true}
	case <-timer.C:
		if p.resultErr != nil {
			return provider.Activation{}, p.resultErr
		}
		return provider.Activation{ID: "late-purchase", Resource: "+12025550123"}, nil
	}
}

func TestPhoneAllocationWaitsForInFlightResultPastStockWindow(t *testing.T) {
	for _, firstStock := range []bool{false, true} {
		for _, stockResult := range []bool{false, true} {
			name := "initial"
			if firstStock {
				name = "retry"
			}
			if stockResult {
				name += "_stock"
			} else {
				name += "_success"
			}
			t.Run(name, func(t *testing.T) {
				window := 100 * time.Millisecond
				p := &responseAfterStockWindow{testProvider: &testProvider{}, window: window, firstStock: firstStock}
				if stockResult {
					p.resultErr = phoneStockError()
				}
				req := provider.Request{Kind: "phone", Service: "dr", Country: "187", ProviderID: "3243", MaxPrice: "0.018", TTL: 20 * time.Minute}
				activation, err, exhausted := retryPhoneAllocation(context.Background(), p, req, window, time.Millisecond)
				wantCalls := 1
				if firstStock {
					wantCalls++
				}
				if p.allocations != wantCalls {
					t.Fatalf("purchases=%d want=%d", p.allocations, wantCalls)
				}
				if stockResult {
					if err != p.resultErr || !exhausted || activation.ID != "" {
						t.Fatalf("late rejection was lost: activation=%+v err=%v exhausted=%v", activation, err, exhausted)
					}
				} else if err != nil || exhausted || activation.ID != "late-purchase" {
					t.Fatalf("late purchase was lost: activation=%+v err=%v exhausted=%v", activation, err, exhausted)
				}
				for i, actual := range p.requests {
					if actual != req || p.deadlines[i] < 24*time.Second {
						t.Fatalf("request changed or timeout shortened: request=%+v deadline=%v", actual, p.deadlines[i])
					}
				}
			})
		}
	}
}

func TestPhoneAllocationWaitsForUnknownResultWithoutRetryingOrHiding(t *testing.T) {
	unknown := &provider.Error{Code: "upstream_unavailable", Uncertain: true}
	p := &responseAfterStockWindow{testProvider: &testProvider{}, window: 10 * time.Millisecond, resultErr: unknown}
	_, err, exhausted := retryPhoneAllocation(context.Background(), p, provider.Request{Kind: "phone"}, p.window, time.Millisecond)
	if err != unknown || exhausted || p.allocations != 1 {
		t.Fatalf("unknown purchase replayed or classified as stock: err=%v exhausted=%v calls=%d", err, exhausted, p.allocations)
	}
}

type cancelledStockProvider struct {
	*testProvider
	cancel context.CancelFunc
}

func (p *cancelledStockProvider) Allocate(ctx context.Context, req provider.Request) (provider.Activation, error) {
	p.allocations++
	p.cancel()
	return provider.Activation{}, phoneStockError()
}

func TestPhoneAllocationShutdownStopsRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &cancelledStockProvider{testProvider: &testProvider{}, cancel: cancel}
	_, err, exhausted := retryPhoneAllocation(ctx, p, provider.Request{Kind: "phone"}, time.Second, time.Millisecond)
	if !errors.Is(err, context.Canceled) || exhausted || p.allocations != 1 {
		t.Fatalf("continued after shutdown: err=%v exhausted=%v calls=%d", err, exhausted, p.allocations)
	}
}

func TestPhoneAllocationActualTimeoutKeepsReviewAndOriginalOrder(t *testing.T) {
	base := &testProvider{}
	a := newTestApp(t, base)
	a.client = &phoneDeadlineProvider{base}
	cookie := adminCookie(t, a)
	parent := a.ctx
	ctx, cancel := context.WithTimeout(parent, 300*time.Millisecond)
	defer cancel()
	a.ctx = ctx
	first := adminAllocate(t, a, cookie, "phone", "timedout-admin-request-01")
	a.ctx = parent
	second := adminAllocate(t, a, cookie, "phone", "timedout-admin-request-02")
	if first.Order.Status != "review" || first.Order.CanRetry || first.Order.Resource != "" || second.Order.ID != first.Order.ID || base.allocations != 1 || len(a.phoneUnavailable) != 0 {
		t.Fatalf("timed-out purchase was replayed or hidden: first=%+v second=%+v calls=%d", first.Order, second.Order, base.allocations)
	}
}
