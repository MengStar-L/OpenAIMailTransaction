package app

import (
	"context"
	"errors"
	"time"

	"openai-mail-transaction/internal/provider"
)

const phoneAllocationWindow = 10 * time.Second
const phoneAllocationRetry = time.Second
const phoneAllocationRequestTimeout = 25 * time.Second

// The durable order remains allocating across attempts. Only an explicit stock
// rejection establishes that no purchase happened and is safe to repeat. The
// original country, channel and price remain pinned for the entire window.
// The stock window limits starting another attempt, never a purchase already
// sent upstream. Every attempt gets its full response timeout, while shutdown
// still cancels the parent context.
func retryPhoneAllocation(parent context.Context, client provider.Client, req provider.Request, window, interval time.Duration) (provider.Activation, error, bool) {
	deadline := time.Now().Add(window)
	var lastStockError error
	for {
		if err := parent.Err(); err != nil {
			return provider.Activation{}, err, false
		}
		if lastStockError != nil && !time.Now().Before(deadline) {
			return provider.Activation{}, lastStockError, true
		}
		ctx, cancel := context.WithTimeout(parent, phoneAllocationRequestTimeout)
		activation, err := client.Allocate(ctx, req)
		cancel()
		var pe *provider.Error
		if err == nil || !errors.As(err, &pe) || pe.Uncertain || pe.Code != "no_stock" {
			// A deadline or transport failure during the purchase remains
			// uncertain, even if earlier attempts were definitely rejected.
			return activation, err, false
		}
		lastStockError = err
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
		case <-timer.C:
			if !time.Now().Before(deadline) {
				return provider.Activation{}, err, parent.Err() == nil
			}
		}
	}
}
