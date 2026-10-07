package provider

import (
	"context"
	"net/url"
	"strings"
)

// BalanceClient is optional and read-only. Amount remains decimal text so the
// administrator sees the upstream value without floating-point rounding.
type BalanceClient interface {
	Balance(context.Context) (Balance, error)
}

type Balance struct {
	Amount string
}

func (b Balance) Valid() bool { return pricePattern.MatchString(b.Amount) }

func (s *SMSBower) Balance(ctx context.Context) (Balance, error) {
	// https://smsbower.app/api/?page=client documents ACCESS_BALANCE:<amount>.
	body, err := s.request(ctx, "/stubs/handler_api.php", url.Values{"action": {"getBalance"}}, false)
	if err != nil {
		return Balance{}, err
	}
	text := strings.TrimSpace(string(body))
	if known := recognizedError(text); known != nil {
		return Balance{}, known
	}
	amount, ok := strings.CutPrefix(text, "ACCESS_BALANCE:")
	result := Balance{Amount: amount}
	if !ok || !result.Valid() {
		return Balance{}, invalidResponse(false)
	}
	return result, nil
}

func (d *Demo) Balance(ctx context.Context) (Balance, error) {
	if ctx.Err() != nil {
		return Balance{}, &Error{Code: "interrupted", Message: "请求已中断，请重试"}
	}
	// The HTTP view labels this explicitly as a simulated demo balance.
	return Balance{Amount: "100.00"}, nil
}
