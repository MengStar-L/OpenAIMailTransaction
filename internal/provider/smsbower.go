package provider

import (
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const DefaultBaseURL = "https://smsbower.page"
const maxResponseBytes = 1024 * 1024

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var pricePattern = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,8})(?:\.[0-9]{1,8})?$`)
var phonePattern = regexp.MustCompile(`^\+?[0-9]{6,18}$`)

type SMSBower struct {
	base   *url.URL
	key    string
	client *http.Client
}

func NewSMSBower(baseURL, apiKey string) (*SMSBower, error) {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	u, err := url.Parse(baseURL)
	if err != nil || u == nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, &Error{Code: "bad_configuration", Message: "资源平台 API 地址无效"}
	}
	// Plain HTTP is restricted to loopback development/test servers.
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || ip != nil && ip.IsLoopback())) {
		return nil, &Error{Code: "bad_configuration", Message: "资源平台 API 地址必须使用 HTTPS"}
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, &Error{Code: "bad_configuration", Message: "尚未配置资源平台 API 密钥"}
	}
	u.Path = ""
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// These GET endpoints can purchase resources. Go may transparently replay
	// an idempotent request after a stale pooled connection fails. Never reuse
	// connections here, so that transport retry cannot purchase twice.
	transport.DisableKeepAlives = true
	return &SMSBower{base: u, key: apiKey, client: &http.Client{
		Timeout:   20 * time.Second,
		Transport: transport,
		// Never forward a credential-bearing query to a redirect destination.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func validateRequest(req Request) bool {
	if !identifierPattern.MatchString(req.Service) || req.TTL <= 0 || req.TTL > 24*time.Hour || !pricePattern.MatchString(req.MaxPrice) {
		return false
	}
	price, ok := new(big.Rat).SetString(req.MaxPrice)
	if !ok || price.Sign() <= 0 {
		return false
	}
	switch req.Kind {
	case "phone":
		return validCatalogID(req.Country, true) && (req.ProviderID == "" || validCatalogID(req.ProviderID, false))
	case "email":
		return req.Domain != "" && len(req.Domain) <= 253 && !strings.ContainsAny(req.Domain, " /\\\r\n\t@?#&")
	default:
		return false
	}
}

func (s *SMSBower) request(ctx context.Context, path string, values url.Values, allocating bool) ([]byte, error) {
	u := *s.base
	u.Path = path
	values.Set("api_key", s.key)
	u.RawQuery = values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, &Error{Code: "request_failed", Message: "无法请求资源平台", Uncertain: allocating}
	}
	req.Header.Set("Accept", "application/json, text/plain")
	resp, err := s.client.Do(req)
	if err != nil {
		// net/url errors include the full query and must never escape this layer.
		return nil, &Error{Code: "upstream_unavailable", Message: "资源平台连接失败，请稍后查看订单", Uncertain: allocating}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &Error{Code: "upstream_http", Message: "资源平台暂不可用，请稍后查看订单", Uncertain: allocating, RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return nil, invalidResponse(allocating)
	}
	return body, nil
}

func retryAfter(raw string) time.Duration {
	if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 86400 {
		return time.Duration(n) * time.Second
	}
	if date, err := http.ParseTime(raw); err == nil {
		if d := time.Until(date); d > 0 && d <= 24*time.Hour {
			return d
		}
	}
	return 0
}

// recognizedError allowlists machine codes; never copy an upstream error string.
func recognizedError(raw string) *Error {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "no_numbers", "no mails yet":
		return &Error{Code: "no_stock", Message: "当前条件下暂无可分配资源，请稍后重试"}
	case "no_balance", "insufficient balance":
		return &Error{Code: "no_balance", Message: "资源平台余额不足，请联系管理员"}
	case "bad_key", "invalid api key", "invalid api_key", "unauthorized":
		return &Error{Code: "bad_key", Message: "资源平台密钥无效，请联系管理员"}
	case "bad_service", "bad_country", "bad_action", "bad_max_price", "wrong_max_price", "no such domain", "max_price_exceeded":
		return &Error{Code: "upstream_configuration", Message: "资源配置或价格上限不可用，请联系管理员"}
	case "no_activation", "pass mail id", "no activation found with such id":
		return &Error{Code: "activation_not_found", Message: "资源平台未找到该订单，请联系管理员核验"}
	case "early_cancel_denied":
		return &Error{Code: "early_cancel", Message: "资源平台尚不允许取消，请稍后重试", RetryAfter: 2 * time.Minute}
	case "bad_status", "bad actual activation status":
		return &Error{Code: "bad_status", Message: "资源平台订单状态已变化，请刷新或联系管理员"}
	}
	return nil
}

func (s *SMSBower) Allocate(ctx context.Context, req Request) (Activation, error) {
	if !validateRequest(req) {
		return Activation{}, invalidRequest()
	}
	start := time.Now().UTC()
	values := url.Values{"service": {req.Service}, "maxPrice": {req.MaxPrice}}
	path := "/api/mail/getActivation"
	if req.Kind == "phone" {
		path = "/stubs/handler_api.php"
		values.Set("action", "getNumberV2")
		values.Set("country", req.Country)
		if req.ProviderID != "" {
			values.Set("providerIds", req.ProviderID)
		}
	} else {
		values.Set("domain", req.Domain)
		values.Set("alias", "0")
	}
	body, err := s.request(ctx, path, values, true)
	if err != nil {
		return Activation{}, err
	}
	if known := recognizedError(string(body)); known != nil {
		return Activation{}, known
	}
	a := Activation{ExpiresAt: start.Add(req.TTL), CancelAfter: start}
	if req.Kind == "phone" {
		// Calculate from response receipt so network time cannot shorten the
		// platform's two-minute minimum cancellation window.
		a.CancelAfter = time.Now().UTC().Add(2 * time.Minute)
		if strings.HasPrefix(strings.TrimSpace(string(body)), "ACCESS_NUMBER:") {
			parts := strings.Split(strings.TrimSpace(string(body)), ":")
			if len(parts) != 3 {
				return Activation{}, invalidResponse(true)
			}
			a.ID, a.Resource = parts[1], parts[2]
		} else {
			object, ok := decodeObject(body)
			if !ok {
				return Activation{}, invalidResponse(true)
			}
			if stringValue(object["error"]) != "" && (stringValue(object["activationId"]) != "" || stringValue(object["phoneNumber"]) != "") {
				return Activation{}, invalidResponse(true)
			}
			if known := recognizedError(stringValue(object["error"])); known != nil {
				return Activation{}, known
			}
			a.ID, a.Resource = stringValue(object["activationId"]), stringValue(object["phoneNumber"])
			a.ExpiresAt = explicitExpiry(object, a.ExpiresAt)
		}
		if !identifierPattern.MatchString(a.ID) || !phonePattern.MatchString(a.Resource) {
			return Activation{}, invalidResponse(true)
		}
		if !strings.HasPrefix(a.Resource, "+") {
			a.Resource = "+" + a.Resource
		}
	} else {
		object, ok := decodeObject(body)
		if !ok {
			return Activation{}, invalidResponse(true)
		}
		if stringValue(object["status"]) == "0" {
			if stringValue(object["mailId"]) != "" || stringValue(object["mail"]) != "" {
				return Activation{}, invalidResponse(true)
			}
			// The documented price refusal is definitive only in a failed mail
			// response without an allocated resource. Never return its raw metadata.
			if strings.EqualFold(strings.TrimSpace(stringValue(object["error"])), "Not available at this price. Try another") {
				return Activation{}, &Error{Code: "price_unavailable", Message: "当前价格上限下暂无可分配邮箱，请检查价格设置"}
			}
			if known := recognizedError(stringValue(object["error"])); known != nil {
				return Activation{}, known
			}
			return Activation{}, invalidResponse(true)
		}
		if stringValue(object["status"]) != "1" {
			return Activation{}, invalidResponse(true)
		}
		a.ID, a.Resource = stringValue(object["mailId"]), stringValue(object["mail"])
		address, err := mail.ParseAddress(a.Resource)
		if !identifierPattern.MatchString(a.ID) || err != nil || address.Address != a.Resource || len(a.Resource) > 320 {
			return Activation{}, invalidResponse(true)
		}
		a.ExpiresAt = explicitExpiry(object, a.ExpiresAt)
	}
	return a, nil
}

func decodeObject(body []byte) (map[string]json.RawMessage, bool) {
	var object map[string]json.RawMessage
	err := json.Unmarshal(body, &object)
	return object, err == nil && object != nil
}

func stringValue(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return string(n)
	}
	return ""
}

// Explicit expiry metadata is accepted as an optional extension. activationTime
// is a creation time and is deliberately never interpreted as an expiry time.
// Missing/invalid metadata uses the caller's local maximum waiting deadline.
func explicitExpiry(object map[string]json.RawMessage, local time.Time) time.Time {
	for _, key := range []string{"expiresAt", "expires_at", "expirationTime", "expiration_time", "activationEndTime"} {
		raw := stringValue(object[key])
		var parsed time.Time
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			parsed = t
		} else if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			if n > 1e12 {
				parsed = time.UnixMilli(n)
			} else {
				parsed = time.Unix(n, 0)
			}
		}
		if !parsed.IsZero() && parsed.Before(local) {
			local = parsed.UTC()
		}
	}
	return local
}

func validateActivation(kind, id string) bool {
	return (kind == "phone" || kind == "email") && identifierPattern.MatchString(id)
}

func cleanCode(code, key string) (string, bool) {
	code = strings.TrimSpace(code)
	// Codes are short printable text. Never return a reflected credential or URL.
	if code == "" || len(code) > 128 || strings.ContainsAny(code, "\r\n\x00") || strings.Contains(code, "://") || strings.Contains(code, key) {
		return "", false
	}
	for _, r := range code {
		if r < 32 || r == 127 {
			return "", false
		}
	}
	return code, true
}

func (s *SMSBower) Poll(ctx context.Context, kind, id string) (Result, error) {
	if !validateActivation(kind, id) {
		return Result{}, invalidRequest()
	}
	values := url.Values{}
	path := "/api/mail/getCode"
	if kind == "phone" {
		path = "/stubs/handler_api.php"
		values.Set("action", "getStatus")
		values.Set("id", id)
	} else {
		values.Set("mailId", id)
	}
	body, err := s.request(ctx, path, values, false)
	if err != nil {
		return Result{}, err
	}
	if kind == "phone" {
		value := strings.TrimSpace(string(body))
		switch value {
		case "STATUS_WAIT_CODE":
			return Result{Status: "waiting"}, nil
		case "STATUS_CANCEL":
			return Result{Status: "cancelled"}, nil
		}
		for _, prefix := range []string{"STATUS_OK:", "STATUS_WAIT_RETRY:"} {
			if strings.HasPrefix(value, prefix) {
				if code, ok := cleanCode(strings.TrimPrefix(value, prefix), s.key); ok {
					return Result{Status: "received", Code: code}, nil
				}
				return Result{}, invalidResponse(false)
			}
		}
		if known := recognizedError(value); known != nil {
			return Result{}, known
		}
		return Result{}, invalidResponse(false)
	}
	object, ok := decodeObject(body)
	if !ok {
		return Result{}, invalidResponse(false)
	}
	if stringValue(object["status"]) == "1" {
		if code, ok := cleanCode(stringValue(object["code"]), s.key); ok {
			return Result{Status: "received", Code: code}, nil
		}
		return Result{}, invalidResponse(false)
	}
	if stringValue(object["status"]) != "0" {
		return Result{}, invalidResponse(false)
	}
	value := stringValue(object["error"])
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "code has not been received yet, please try again later":
		return Result{Status: "waiting"}, nil
	case "activation is already canceled", "activation is already cancelled":
		return Result{Status: "cancelled"}, nil
	}
	if known := recognizedError(value); known != nil {
		return Result{}, known
	}
	return Result{}, invalidResponse(false)
}

func (s *SMSBower) Cancel(ctx context.Context, kind, id string) error {
	return s.setStatus(ctx, kind, id, true)
}

func (s *SMSBower) Complete(ctx context.Context, kind, id string) error {
	return s.setStatus(ctx, kind, id, false)
}

// CompletionStatus uses the read-only endpoint linked from the official mail
// API's Postman collection. A getCode success only repeats a received code and
// cannot establish whether a completion request has already been applied.
func (s *SMSBower) CompletionStatus(ctx context.Context, kind, id string) (Result, error) {
	if kind != "email" || !validateActivation(kind, id) {
		return Result{}, invalidRequest()
	}
	body, err := s.request(ctx, "/api/mail/getStatus", url.Values{"id": {id}}, false)
	if err != nil {
		return Result{}, err
	}
	object, ok := decodeObject(body)
	if !ok {
		return Result{}, invalidResponse(false)
	}
	if stringValue(object["status"]) == "0" {
		if known := recognizedError(stringValue(object["error"])); known != nil {
			return Result{}, known
		}
		return Result{}, invalidResponse(false)
	}
	if stringValue(object["status"]) != "1" || !emptyResponseError(object) {
		return Result{}, invalidResponse(false)
	}
	data, ok := decodeObject(object["data"])
	if !ok || !emptyResponseError(data) {
		return Result{}, invalidResponse(false)
	}
	state := stringValue(data["status"])
	// Reject missing, fractional, negative and unbounded states. The collection
	// does not enumerate every intermediate value, so those cannot prove closure.
	number, err := strconv.ParseUint(state, 10, 8)
	if err != nil || state != strconv.FormatUint(number, 10) {
		return Result{}, invalidResponse(false)
	}
	switch number {
	case 3:
		return Result{Status: "completed"}, nil
	default:
		return Result{Status: "waiting"}, nil
	}
}

// The main API page uses Success for every transition; its linked Postman
// collection documents operation-specific acknowledgments. Do not accept one
// operation's acknowledgment as proof of a different operation's completion.
func mailTransitionAcknowledged(object map[string]json.RawMessage, expected string) bool {
	if stringValue(object["status"]) != "1" || !emptyResponseError(object) {
		return false
	}
	message := strings.TrimSpace(stringValue(object["message"]))
	return strings.EqualFold(message, "success") || strings.EqualFold(message, expected)
}

func emptyResponseError(object map[string]json.RawMessage) bool {
	raw, exists := object["error"]
	if !exists || string(raw) == "null" {
		return true
	}
	var value string
	return json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) == ""
}

func (s *SMSBower) NextCode(ctx context.Context, kind, id string) error {
	if kind != "email" || !validateActivation(kind, id) {
		return invalidRequest()
	}
	body, err := s.request(ctx, "/api/mail/setStatus", url.Values{"id": {id}, "status": {"5"}}, true)
	if err != nil {
		return err
	}
	object, ok := decodeObject(body)
	if !ok {
		return invalidResponse(true)
	}
	if mailTransitionAcknowledged(object, "Wait for next code") {
		return nil
	}
	if stringValue(object["status"]) == "0" {
		if known := recognizedError(stringValue(object["error"])); known != nil {
			// A documented rejection is distinct from a transport failure or
			// malformed acknowledgment, which leaves the transition uncertain.
			return known
		}
	}
	return invalidResponse(true)
}

func (s *SMSBower) setStatus(ctx context.Context, kind, id string, cancel bool) error {
	if !validateActivation(kind, id) {
		return invalidRequest()
	}
	values := url.Values{"id": {id}}
	path, status := "/api/mail/setStatus", "3"
	if cancel {
		status = "2"
	}
	if kind == "phone" {
		path, status = "/stubs/handler_api.php", "6"
		values.Set("action", "setStatus")
		if cancel {
			status = "8"
		}
	}
	values.Set("status", status)
	body, err := s.request(ctx, path, values, false)
	if err != nil {
		return err
	}
	if kind == "phone" {
		value := strings.TrimSpace(string(body))
		if cancel && (value == "ACCESS_CANCEL" || value == "STATUS_CANCEL") || !cancel && value == "ACCESS_ACTIVATION" {
			return nil
		}
		if known := recognizedError(value); known != nil {
			return known
		}
		return invalidResponse(false)
	}
	object, ok := decodeObject(body)
	if !ok {
		return invalidResponse(false)
	}
	expected := "Activation succeed"
	if cancel {
		expected = "Success"
	}
	if mailTransitionAcknowledged(object, expected) {
		return nil
	}
	if stringValue(object["status"]) != "0" {
		return invalidResponse(false)
	}
	value := strings.ToLower(strings.TrimSpace(stringValue(object["error"])))
	if cancel && (value == "activation is already canceled" || value == "activation is already cancelled") {
		return nil
	}
	if known := recognizedError(value); known != nil {
		return known
	}
	return invalidResponse(false)
}
