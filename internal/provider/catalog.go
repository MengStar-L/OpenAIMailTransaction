package provider

import (
	"context"
	"encoding/json"
	"math/big"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// PhoneCatalogClient is a read-only, optional catalogue capability. A listed
// channel is a recent quote, not a reservation; Allocate always keeps maxPrice.
type PhoneCatalogClient interface {
	PhoneCountries(context.Context) ([]PhoneCountry, error)
	PhoneChannels(context.Context, Request) ([]PhoneChannel, error)
}

type PhoneCountry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type PhoneChannel struct {
	Country     string `json:"country"`
	CountryName string `json:"country_name"`
	ProviderID  string `json:"provider_id"`
	Price       string `json:"price"`
	Count       int    `json:"count"`
	Tier        string `json:"tier"`
	TierStatus  string `json:"tier_status"`
}

type catalogCountry struct {
	PhoneCountry
	English string
}

func validCatalogID(value string, allowZero bool) bool {
	if value == "" || len(value) > 10 || len(value) > 1 && value[0] == '0' {
		return false
	}
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	n, err := strconv.ParseUint(value, 10, 32)
	return err == nil && (allowZero || n > 0)
}

func catalogFilter(req Request) (*big.Rat, map[string]bool, bool) {
	if req.Kind != "phone" || !identifierPattern.MatchString(req.Service) || !pricePattern.MatchString(req.MaxPrice) || len(req.Country) > 4096 {
		return nil, nil, false
	}
	price, ok := new(big.Rat).SetString(req.MaxPrice)
	if !ok || price.Sign() <= 0 {
		return nil, nil, false
	}
	countries := make(map[string]bool)
	if req.Country != "" {
		for _, raw := range strings.Split(req.Country, ",") {
			id := strings.TrimSpace(raw)
			if !validCatalogID(id, true) {
				return nil, nil, false
			}
			countries[id] = true
		}
	}
	return price, countries, true
}

func cleanCountryName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 256 {
		return ""
	}
	for _, ch := range value {
		if unicode.IsControl(ch) {
			return ""
		}
	}
	return value
}

func parsePhoneCountries(body []byte) ([]catalogCountry, error) {
	if known := recognizedError(string(body)); known != nil {
		return nil, known
	}
	rows := make([]json.RawMessage, 0)
	if object, ok := decodeObject(body); ok {
		if known := recognizedError(stringValue(object["error"])); known != nil {
			return nil, known
		}
		for _, value := range object {
			rows = append(rows, value)
		}
	} else if err := json.Unmarshal(body, &rows); err != nil || rows == nil {
		return nil, invalidResponse(false)
	}
	countries := make([]catalogCountry, 0, len(rows))
	seen := make(map[string]bool)
	for _, raw := range rows {
		row, ok := decodeObject(raw)
		if !ok {
			continue
		}
		id := stringValue(row["id"])
		if !validCatalogID(id, true) || seen[id] {
			continue
		}
		eng := cleanCountryName(stringValue(row["eng"]))
		name := cleanCountryName(stringValue(row["chn"]))
		if name == "" {
			name = eng
		}
		if name == "" {
			name = cleanCountryName(stringValue(row["rus"]))
		}
		if name == "" {
			name = id
		}
		countries = append(countries, catalogCountry{PhoneCountry: PhoneCountry{ID: id, Name: name}, English: eng})
		seen[id] = true
	}
	if len(countries) == 0 && len(rows) > 0 {
		return nil, invalidResponse(false)
	}
	sort.Slice(countries, func(i, j int) bool {
		a, _ := strconv.ParseUint(countries[i].ID, 10, 32)
		b, _ := strconv.ParseUint(countries[j].ID, 10, 32)
		return a < b
	})
	return countries, nil
}

func (s *SMSBower) phoneCountries(ctx context.Context) ([]catalogCountry, error) {
	body, err := s.request(ctx, "/stubs/handler_api.php", url.Values{"action": {"getCountries"}}, false)
	if err != nil {
		return nil, err
	}
	return parsePhoneCountries(body)
}

func (s *SMSBower) PhoneCountries(ctx context.Context) ([]PhoneCountry, error) {
	countries, err := s.phoneCountries(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]PhoneCountry, 0, len(countries))
	for _, c := range countries {
		result = append(result, c.PhoneCountry)
	}
	return result, nil
}

func explicitPhoneTier(row map[string]json.RawMessage) (string, string) {
	// Never infer quality from price, stock, numeric ranks, or missing fields.
	status := "not_provided"
	for _, field := range []string{"tier", "rank", "quality"} {
		switch tier := strings.ToLower(strings.TrimSpace(stringValue(row[field]))); tier {
		case "bronze", "silver", "gold":
			return tier, "provided"
		case "", "unknown":
		default:
			status = "unavailable"
		}
	}
	return "unknown", status
}

func parsePhoneChannels(body []byte, req Request, limit *big.Rat, allowed map[string]bool, countries []catalogCountry) ([]PhoneChannel, error) {
	if known := recognizedError(string(body)); known != nil {
		if known.Code == "no_stock" {
			return []PhoneChannel{}, nil
		}
		return nil, known
	}
	root, ok := decodeObject(body)
	if !ok {
		return nil, invalidResponse(false)
	}
	if known := recognizedError(stringValue(root["error"])); known != nil {
		return nil, known
	}
	if _, hasError := root["error"]; hasError {
		return nil, invalidResponse(false)
	}
	for key := range root {
		if !validCatalogID(key, true) {
			return nil, invalidResponse(false)
		}
	}
	names := make(map[string]string, len(countries))
	for _, c := range countries {
		names[c.ID] = c.Name
	}
	channels := make([]PhoneChannel, 0)
	for countryID, rawCountry := range root {
		if !validCatalogID(countryID, true) || len(allowed) > 0 && !allowed[countryID] {
			continue
		}
		country, ok := decodeObject(rawCountry)
		if !ok {
			continue
		}
		service, ok := decodeObject(country[req.Service])
		if !ok {
			continue
		}
		for providerID, rawChannel := range service {
			row, ok := decodeObject(rawChannel)
			if !ok || !validCatalogID(providerID, false) {
				continue
			}
			// The keyed ID and explicit ID must agree; otherwise pinning a
			// channel could buy a different product than the displayed quote.
			if explicitID := stringValue(row["provider_id"]); explicitID != providerID {
				continue
			}
			countText := stringValue(row["count"])
			if !validCatalogID(countText, false) {
				continue
			}
			count, err := strconv.ParseUint(countText, 10, 31)
			priceText := stringValue(row["price"])
			if err != nil || !pricePattern.MatchString(priceText) {
				continue
			}
			price, ok := new(big.Rat).SetString(priceText)
			if !ok || price.Sign() <= 0 || price.Cmp(limit) > 0 {
				continue
			}
			name := names[countryID]
			if name == "" {
				name = countryID
			}
			tier, tierStatus := explicitPhoneTier(row)
			channels = append(channels, PhoneChannel{Country: countryID, CountryName: name, ProviderID: providerID, Price: priceText, Count: int(count), Tier: tier, TierStatus: tierStatus})
		}
	}
	sortPhoneChannels(channels)
	return channels, nil
}

func sortPhoneChannels(channels []PhoneChannel) {
	sort.Slice(channels, func(i, j int) bool {
		a, _ := new(big.Rat).SetString(channels[i].Price)
		b, _ := new(big.Rat).SetString(channels[j].Price)
		if n := a.Cmp(b); n != 0 {
			return n > 0
		}
		if channels[i].Country != channels[j].Country {
			return channels[i].Country < channels[j].Country
		}
		return channels[i].ProviderID < channels[j].ProviderID
	})
}

func countrySlug(name string) string {
	return strings.Trim(strings.Map(func(ch rune) rune {
		if ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' {
			return ch
		}
		if ch == ' ' || ch == '-' {
			return '-'
		}
		return -1
	}, strings.ToLower(name)), "-")
}

func addGoldPhoneTiers(channels []PhoneChannel, body []byte, countries []catalogCountry) error {
	// The documented top-countries endpoint only identifies Gold providers in
	// a partial country list. Missing entries remain unknown, never Bronze.
	top, ok := decodeObject(body)
	if !ok {
		return invalidResponse(false)
	}
	if _, hasError := top["error"]; hasError {
		return invalidResponse(false)
	}
	aliases := make(map[string]string)
	for _, c := range countries {
		aliases[c.ID] = c.ID
		if slug := countrySlug(c.English); slug != "" {
			if previous, exists := aliases[slug]; exists && previous != c.ID {
				aliases[slug] = ""
			} else {
				aliases[slug] = c.ID
			}
		}
	}
	gold := make(map[string]bool)
	var metadataErr error
	for alias, raw := range top {
		country := aliases[alias]
		if validCatalogID(alias, true) {
			country = alias
		}
		if country == "" {
			metadataErr = invalidResponse(false)
			continue
		}
		providers, ok := decodeObject(raw)
		if !ok {
			metadataErr = invalidResponse(false)
			continue
		}
		for id, rawProvider := range providers {
			provider, ok := decodeObject(rawProvider)
			if !ok || !validCatalogID(id, false) || !pricePattern.MatchString(stringValue(provider["price"])) || !validCatalogID(stringValue(provider["count"]), true) {
				metadataErr = invalidResponse(false)
				continue
			}
			if explicitID, exists := provider["provider_id"]; exists && stringValue(explicitID) != id {
				metadataErr = invalidResponse(false)
				continue
			}
			gold[country+":"+id] = true
		}
	}
	for i := range channels {
		if channels[i].Tier == "unknown" && gold[channels[i].Country+":"+channels[i].ProviderID] {
			channels[i].Tier = "gold"
			channels[i].TierStatus = "provided"
		}
	}
	return metadataErr
}

func (s *SMSBower) PhoneChannels(ctx context.Context, req Request) ([]PhoneChannel, error) {
	limit, allowed, ok := catalogFilter(req)
	if !ok {
		return nil, invalidRequest()
	}
	values := url.Values{"action": {"getPricesV3"}, "service": {req.Service}}
	// Multiple country IDs are not documented for getPricesV3. Fetch all
	// countries once and filter locally rather than relying on undefined API.
	if len(allowed) == 1 {
		for country := range allowed {
			values.Set("country", country)
		}
	}
	body, err := s.request(ctx, "/stubs/handler_api.php", values, false)
	if err != nil {
		return nil, err
	}
	channels, err := parsePhoneChannels(body, req, limit, allowed, nil)
	if err != nil || len(channels) == 0 {
		return channels, err
	}
	// Fetch optional metadata in parallel with independent deadlines. Slow
	// country names must not consume the entire Gold request's time budget.
	var countries []catalogCountry
	var goldBody []byte
	var goldErr error
	var metadata sync.WaitGroup
	metadata.Add(2)
	go func() {
		defer metadata.Done()
		metaCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		countries, _ = s.phoneCountries(metaCtx)
	}()
	go func() {
		defer metadata.Done()
		metaCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		goldBody, goldErr = s.request(metaCtx, "/stubs/handler_api.php", url.Values{"action": {"getTopCountriesByService"}, "service": {req.Service}}, false)
	}()
	metadata.Wait()
	names := make(map[string]string, len(countries))
	for _, c := range countries {
		names[c.ID] = c.Name
	}
	if goldErr == nil {
		goldErr = addGoldPhoneTiers(channels, goldBody, countries)
	}
	for i := range channels {
		if name := names[channels[i].Country]; name != "" {
			channels[i].CountryName = name
		}
		if channels[i].Tier == "unknown" && goldErr != nil {
			channels[i].TierStatus = "unavailable"
		}
	}
	return channels, nil
}

func (d *Demo) PhoneCountries(ctx context.Context) ([]PhoneCountry, error) {
	if ctx.Err() != nil {
		return nil, &Error{Code: "interrupted", Message: "请求已中断，请重试"}
	}
	return []PhoneCountry{{ID: "0", Name: "俄罗斯"}, {ID: "36", Name: "加拿大"}, {ID: "187", Name: "美国"}}, nil
}

func (d *Demo) PhoneChannels(ctx context.Context, req Request) ([]PhoneChannel, error) {
	limit, allowed, ok := catalogFilter(req)
	if !ok {
		return nil, invalidRequest()
	}
	countries, err := d.PhoneCountries(ctx)
	if err != nil {
		return nil, err
	}
	channels := make([]PhoneChannel, 0)
	for _, country := range countries {
		if len(allowed) > 0 && !allowed[country.ID] {
			continue
		}
		for index, tier := range []string{"bronze", "silver", "gold"} {
			prices := []string{"0.10", "0.15", "0.20"}
			price, _ := new(big.Rat).SetString(prices[index])
			if price.Cmp(limit) > 0 {
				continue
			}
			channels = append(channels, PhoneChannel{Country: country.ID, CountryName: country.Name, ProviderID: strconv.Itoa(1001 + index), Price: prices[index], Count: 128 - index*24, Tier: tier, TierStatus: "provided"})
		}
	}
	sortPhoneChannels(channels)
	return channels, nil
}
