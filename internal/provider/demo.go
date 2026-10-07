package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Demo embeds its start time and deadline in each identifier; an application
// restart does not reset waiting time or lose an already available demo code.
type Demo struct{}

func NewDemo() Client { return &Demo{} }

func (d *Demo) Allocate(ctx context.Context, req Request) (Activation, error) {
	if err := ctx.Err(); err != nil {
		return Activation{}, &Error{Code: "interrupted", Message: "请求已中断，请重试"}
	}
	if (req.Kind != "phone" && req.Kind != "email") || req.TTL <= 0 || req.TTL > 24*time.Hour {
		return Activation{}, invalidRequest()
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Activation{}, &Error{Code: "demo_unavailable", Message: "演示资源暂不可用"}
	}
	now := time.Now().UTC()
	id := fmt.Sprintf("d1.%s.%d.%d.%s", req.Kind, now.UnixNano(), req.TTL.Nanoseconds(), hex.EncodeToString(nonce[:]))
	resource := fmt.Sprintf("+120255501%02d", int(nonce[0])%100)
	if req.Kind == "email" {
		resource = "demo-" + hex.EncodeToString(nonce[:4]) + "@example.invalid"
	}
	return Activation{ID: id, Resource: resource, ExpiresAt: now.Add(req.TTL), CancelAfter: now.Add(10 * time.Second)}, nil
}

func demoTimes(kind, id string) (time.Time, time.Time, error) {
	parts := strings.Split(id, ".")
	if len(parts) != 5 || parts[0] != "d1" || parts[1] != kind || (kind != "phone" && kind != "email") {
		return time.Time{}, time.Time{}, invalidRequest()
	}
	start, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || start <= 0 {
		return time.Time{}, time.Time{}, invalidRequest()
	}
	ttl, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || ttl <= 0 || ttl > int64(24*time.Hour) {
		return time.Time{}, time.Time{}, invalidRequest()
	}
	if n, err := hex.DecodeString(parts[4]); err != nil || len(n) != 8 {
		return time.Time{}, time.Time{}, invalidRequest()
	}
	t := time.Unix(0, start).UTC()
	return t, t.Add(time.Duration(ttl)), nil
}

func (d *Demo) Poll(ctx context.Context, kind, id string) (Result, error) {
	return d.PollRound(ctx, kind, id, 1)
}

func (d *Demo) PollRound(ctx context.Context, kind, id string, round int) (Result, error) {
	if ctx.Err() != nil {
		return Result{}, &Error{Code: "interrupted", Message: "请求已中断，请重试"}
	}
	if round < 1 || round > 3 || kind != "email" && round != 1 {
		return Result{}, invalidRequest()
	}
	start, expires, err := demoTimes(kind, id)
	if err != nil {
		return Result{}, err
	}
	// Once issued, a demo code remains consumable until the app completes it,
	// exactly as the app persists a received live code across its local deadline.
	readyAfter := time.Duration(round) * 15 * time.Second
	if expires.Sub(start) >= readyAfter && !time.Now().Before(start.Add(readyAfter)) {
		codes := [...]string{"628391", "042716", "905238"}
		return Result{Status: "received", Code: codes[round-1]}, nil
	}
	if !time.Now().Before(expires) {
		return Result{Status: "expired"}, nil
	}
	return Result{Status: "waiting"}, nil
}

func (d *Demo) NextCode(ctx context.Context, kind, id string) error {
	if kind != "email" {
		return invalidRequest()
	}
	_, expires, err := demoTimes(kind, id)
	if err != nil {
		return err
	}
	if !time.Now().Before(expires) {
		return &Error{Code: "bad_status", Message: "邮箱已到期"}
	}
	// Receipt rounds are persisted by the app and supplied to PollRound. This
	// acknowledgment keeps no transient state that could be lost on restart.
	return d.Complete(ctx, kind, id)
}

func (d *Demo) Cancel(ctx context.Context, kind, id string) error {
	if ctx.Err() != nil {
		return &Error{Code: "interrupted", Message: "请求已中断，请重试"}
	}
	start, expires, err := demoTimes(kind, id)
	if err != nil {
		return err
	}
	if remaining := time.Until(start.Add(10 * time.Second)); remaining > 0 {
		return &Error{Code: "early_cancel", Message: "请等待取消倒计时结束", RetryAfter: remaining}
	}
	if expires.Sub(start) >= 15*time.Second && !time.Now().Before(start.Add(15*time.Second)) {
		return &Error{Code: "bad_status", Message: "验证码已送达，请刷新订单后完成"}
	}
	return nil
}

func (d *Demo) Complete(ctx context.Context, kind, id string) error {
	result, err := d.Poll(ctx, kind, id)
	if err != nil {
		return err
	}
	if result.Status != "received" {
		return &Error{Code: "code_required", Message: "收到验证码后才能完成订单"}
	}
	return nil
}
