// Package provider implements temporary, single-activation resource providers.
package provider

import (
	"context"
	"time"
)

type Client interface {
	Allocate(context.Context, Request) (Activation, error)
	Poll(context.Context, string, string) (Result, error)
	Cancel(context.Context, string, string) error
	Complete(context.Context, string, string) error
}

// NextCodeClient is optional: only providers that can reuse an active mailbox
// implement it. A successful call starts the next receipt round; the app must
// persist its intent first and must not replay an uncertain transition.
type NextCodeClient interface {
	NextCode(context.Context, string, string) error
}

// CompletionStatusClient optionally reads the activation state without repeating
// a closing request. Only an explicit completed/cancelled state proves closure;
// waiting means the lookup did not establish a terminal state, not that another
// state-changing request is safe. It does not return previously received codes.
type CompletionStatusClient interface {
	CompletionStatus(context.Context, string, string) (Result, error)
}

// RoundPollClient lets the stateless demo recover a persisted receipt round
// after a restart. Live providers keep their receipt round upstream and use Poll.
type RoundPollClient interface {
	PollRound(context.Context, string, string, int) (Result, error)
}

// InventoryClient exposes read-only mailbox stock for the configured service,
// domain, and price ceiling. An error means unknown availability, not zero.
type InventoryClient interface {
	EmailInventory(context.Context, Request) (EmailInventory, error)
}

type EmailInventory struct {
	Count int
}

type Request struct {
	Kind, Service, Country, Domain, MaxPrice string
	ProviderID                               string
	TTL                                      time.Duration
}

type Activation struct {
	ID, Resource           string
	ExpiresAt, CancelAfter time.Time
}

type Result struct {
	Status, Code string
}

// Error never contains an upstream URL, response body, API key, or raw transport
// error. Uncertain means a state-changing request may have succeeded and must
// not be replayed or refunded automatically. RetryAfter is advisory, not
// confirmation of release.
type Error struct {
	Code, Message string
	Uncertain     bool
	RetryAfter    time.Duration
	// DiagnosticReason is a bounded internal classification, never a wrapped
	// transport error or URL. It is excluded from public JSON error responses.
	DiagnosticReason string `json:"-"`
}

func (e *Error) Error() string { return e.Message }

func invalidRequest() *Error {
	return &Error{Code: "invalid_request", Message: "资源参数未配置完整，请联系管理员"}
}

func invalidResponse(allocation bool) *Error {
	return &Error{Code: "invalid_response", Message: "资源平台返回异常，请稍后查看订单或联系管理员", Uncertain: allocation}
}
