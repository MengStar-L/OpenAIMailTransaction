package provider

import (
	"context"
	"math/big"
	"net/url"
	"strconv"
	"strings"
)

func inventoryPrice(req Request) (*big.Rat, bool) {
	if req.Kind != "email" || !identifierPattern.MatchString(req.Service) || req.Domain == "" || len(req.Domain) > 253 || strings.ContainsAny(req.Domain, " /\\\r\n\t@?#&") || !pricePattern.MatchString(req.MaxPrice) {
		return nil, false
	}
	price, ok := new(big.Rat).SetString(req.MaxPrice)
	return price, ok && price.Sign() > 0
}

func (s *SMSBower) EmailInventory(ctx context.Context, req Request) (EmailInventory, error) {
	limit, ok := inventoryPrice(req)
	if !ok {
		return EmailInventory{}, invalidRequest()
	}
	// The official stock endpoint has no documented alias or maxPrice option.
	// Select the same service/domain as allocation and apply the price locally.
	body, err := s.request(ctx, "/api/mail/getPriceRests", url.Values{"service": {req.Service}, "domain": {req.Domain}}, false)
	if err != nil {
		return EmailInventory{}, err
	}
	if known := recognizedError(string(body)); known != nil {
		if known.Code == "no_stock" {
			return EmailInventory{Count: 0}, nil
		}
		return EmailInventory{}, known
	}
	object, ok := decodeObject(body)
	if !ok {
		return EmailInventory{}, invalidResponse(false)
	}
	if stringValue(object["status"]) == "0" {
		// A response cannot both deny the request and provide inventory.
		if _, exists := object["data"]; exists {
			return EmailInventory{}, invalidResponse(false)
		}
		if known := recognizedError(stringValue(object["error"])); known != nil {
			if known.Code == "no_stock" {
				return EmailInventory{Count: 0}, nil
			}
			return EmailInventory{}, known
		}
		return EmailInventory{}, invalidResponse(false)
	}
	if stringValue(object["status"]) != "1" || stringValue(object["error"]) != "" {
		return EmailInventory{}, invalidResponse(false)
	}
	data, ok := decodeObject(object["data"])
	if !ok {
		return EmailInventory{}, invalidResponse(false)
	}
	service, ok := decodeObject(data[req.Service])
	if !ok {
		return EmailInventory{}, invalidResponse(false)
	}
	domain, ok := decodeObject(service[req.Domain])
	if !ok {
		return EmailInventory{}, invalidResponse(false)
	}
	// Bound counts to a signed 32-bit integer; this also remains exact in every
	// supported Go architecture and in the browser. Reject fractional counts,
	// signs, whitespace, and overflowing/implausibly large upstream values.
	countText := stringValue(domain["count"])
	if countText == "" || len(countText) > 10 || len(countText) > 1 && countText[0] == '0' {
		return EmailInventory{}, invalidResponse(false)
	}
	for _, c := range countText {
		if c < '0' || c > '9' {
			return EmailInventory{}, invalidResponse(false)
		}
	}
	count, err := strconv.ParseUint(countText, 10, 31)
	priceText := stringValue(domain["price"])
	if err != nil || !pricePattern.MatchString(priceText) {
		return EmailInventory{}, invalidResponse(false)
	}
	price, ok := new(big.Rat).SetString(priceText)
	if !ok || price.Sign() < 0 {
		return EmailInventory{}, invalidResponse(false)
	}
	if price.Cmp(limit) > 0 {
		return EmailInventory{Count: 0}, nil
	}
	return EmailInventory{Count: int(count)}, nil
}

func (d *Demo) EmailInventory(ctx context.Context, req Request) (EmailInventory, error) {
	if ctx.Err() != nil {
		return EmailInventory{}, &Error{Code: "interrupted", Message: "请求已中断，请重试"}
	}
	if req.Kind != "email" {
		return EmailInventory{}, invalidRequest()
	}
	// This is explicitly simulated stock, independent of paid upstream APIs,
	// reservations, and process lifetime, just like the other demo operations.
	return EmailInventory{Count: 128}, nil
}
