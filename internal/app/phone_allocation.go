package app

import (
	"context"
	"errors"
	"time"

	"openai-mail-transaction/internal/provider"
)

const phoneAllocationWindow = 10 * time.Second
const phoneAllocationRetry = time.Second

// The durable order remains allocating across attempts. Only an explicit stock
// rejection establishes that no purchase happened and is safe to repeat. The
// original country, channel and price remain pinned for the entire window.
func retryPhoneAllocation(parent context.Context, client provider.Client, req provider.Request, window, interval time.Duration) (provider.Activation, error, bool) {
	ctx, cancel := context.WithTimeout(parent, window)
	defer cancel()
	var lastStockError error
	for {
		if err := ctx.Err(); err != nil {
			if lastStockError != nil && parent.Err() == nil {
				return provider.Activation{}, lastStockError, true
			}
			return provider.Activation{}, err, false
		}
		activation, err := client.Allocate(ctx, req)
		var pe *provider.Error
		if err == nil || !errors.As(err, &pe) || pe.Uncertain || pe.Code != "no_stock" {
			// A deadline or transport failure during the purchase remains
			// uncertain, even if earlier attempts were definitely rejected.
			return activation, err, false
		}
		lastStockError = err
		deadline, _ := ctx.Deadline()
		remaining := time.Until(deadline)
		if remaining <= 0 && parent.Err() == nil {
			return provider.Activation{}, err, true
		}
		wait := min(interval, remaining)
		timer := time.NewTimer(max(wait, 0))
		select {
		case <-parent.Done():
			timer.Stop()
			return provider.Activation{}, parent.Err(), false
		case <-ctx.Done():
			timer.Stop()
			return provider.Activation{}, err, parent.Err() == nil
		case <-timer.C:
			if !time.Now().Before(deadline) {
				return provider.Activation{}, err, parent.Err() == nil
			}
		}
	}
}
