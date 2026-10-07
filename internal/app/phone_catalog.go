package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"openai-mail-transaction/internal/provider"
)

type phoneSelection struct {
	Country    string `json:"phone_country"`
	ProviderID string `json:"phone_provider_id"`
}

type phoneCatalogCache struct {
	Channels  []provider.PhoneChannel
	ExpiresAt time.Time
}

const phoneUnavailableTTL = 10 * time.Minute
const phoneUnavailableLimit = 1024
const phoneUnavailableMessage = "10 秒内未获取到号码，该渠道暂时隐藏，10 分钟后恢复，请选择其他渠道"

func (a *App) loadPhoneUnavailable() error {
	if _, err := a.db.Exec("DELETE FROM phone_unavailable WHERE expires_at<=?", millis(time.Now())); err != nil {
		return err
	}
	rows, err := a.db.Query("SELECT credential,service,country,provider_id,expires_at FROM phone_unavailable ORDER BY expires_at DESC LIMIT ?", phoneUnavailableLimit)
	if err != nil {
		return err
	}
	defer rows.Close()
	a.phoneUnavailable = make(map[[4]string]time.Time)
	for rows.Next() {
		var key [4]string
		var expiry int64
		if err := rows.Scan(&key[0], &key[1], &key[2], &key[3], &expiry); err != nil {
			return err
		}
		a.phoneUnavailable[key] = stamp(expiry)
	}
	return rows.Err()
}

// A stock rejection is authoritative for this credential and exact channel,
// even when the upstream reference catalog still advertises positive stock.
// Price is deliberately excluded: changing a ceiling cannot bypass the pause.
func (a *App) markPhoneUnavailable(req provider.Request) error {
	if req.Kind != "phone" || req.Country == "" || req.ProviderID == "" {
		return nil
	}
	credential := hash(a.currentAPIKey())
	now := time.Now()
	a.phoneCatalogMu.Lock()
	defer a.phoneCatalogMu.Unlock()
	key := [4]string{credential, req.Service, req.Country, req.ProviderID}
	expires := now.Add(phoneUnavailableTTL)
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM phone_unavailable WHERE expires_at<=?", millis(now)); err == nil {
		_, err = tx.Exec(`INSERT INTO phone_unavailable(credential,service,country,provider_id,expires_at) VALUES(?,?,?,?,?)
 ON CONFLICT(credential,service,country,provider_id) DO UPDATE SET expires_at=excluded.expires_at`, key[0], key[1], key[2], key[3], millis(expires))
	}
	if err == nil {
		_, err = tx.Exec("DELETE FROM phone_unavailable WHERE rowid IN (SELECT rowid FROM phone_unavailable ORDER BY expires_at DESC LIMIT -1 OFFSET ?)", phoneUnavailableLimit)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return err
	}
	for key := range a.phoneCatalogCache {
		if key[0] == credential && key[1] == req.Service {
			delete(a.phoneCatalogCache, key)
		}
	}
	for key, until := range a.phoneUnavailable {
		if !now.Before(until) {
			delete(a.phoneUnavailable, key)
		}
	}
	if a.phoneUnavailable == nil {
		a.phoneUnavailable = make(map[[4]string]time.Time)
	}
	if _, exists := a.phoneUnavailable[key]; !exists && len(a.phoneUnavailable) >= phoneUnavailableLimit {
		var earliest [4]string
		var expiry time.Time
		for candidate, until := range a.phoneUnavailable {
			if expiry.IsZero() || until.Before(expiry) {
				earliest, expiry = candidate, until
			}
		}
		delete(a.phoneUnavailable, earliest)
	}
	a.phoneUnavailable[key] = expires
	return nil
}

func (a *App) isPhoneUnavailable(service string, choice phoneSelection) bool {
	key := [4]string{hash(a.currentAPIKey()), service, choice.Country, choice.ProviderID}
	a.phoneCatalogMu.Lock()
	defer a.phoneCatalogMu.Unlock()
	return time.Now().Before(a.phoneUnavailable[key])
}

// Called with phoneCatalogMu held. Always return a separate slice so filtering
// never mutates cached reference stock, which is usable again after the pause.
func (a *App) availablePhoneChannels(channels []provider.PhoneChannel, credential, service string) []provider.PhoneChannel {
	available := make([]provider.PhoneChannel, 0, len(channels))
	now := time.Now()
	for _, channel := range channels {
		key := [4]string{credential, service, channel.Country, channel.ProviderID}
		if !now.Before(a.phoneUnavailable[key]) {
			available = append(available, channel)
		}
	}
	return available
}

func insertPhoneChannel(tx *sql.Tx, orderID string, channel *provider.PhoneChannel) error {
	if channel == nil {
		return nil
	}
	data, err := json.Marshal(channel)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO order_phone_channels(order_id,channel_json) VALUES(?,?)", orderID, string(data))
	return err
}

func (a *App) decoratePhoneChannel(o *Order) {
	if o.Kind != "phone" {
		return
	}
	var raw string
	if a.db.QueryRow("SELECT channel_json FROM order_phone_channels WHERE order_id=?", o.ID).Scan(&raw) != nil {
		return
	}
	var channel provider.PhoneChannel
	if json.Unmarshal([]byte(raw), &channel) == nil {
		o.PhoneChannel = &channel
	}
}

// Keep the existing setting and voucher snapshots compatible with single-country
// releases. An explicit asterisk, never a missing configuration, means all.
func normalizePhoneCountries(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "*" {
		return raw, nil
	}
	if len(raw) > 4096 {
		return "", errors.New("国家代码过多")
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '，' || r == '、' || r == ';' || r == '；' || r == ' ' || r == '\n' || r == '\t' || r == '\r'
	})
	if len(parts) == 0 || len(parts) > 300 {
		return "", errors.New("请填写国家代码，或选择全部国家")
	}
	seen := map[string]bool{}
	ids := []string{}
	for _, id := range parts {
		if !countryPattern.MatchString(id) {
			return "", errors.New("国家代码应为数字，多个代码用逗号分隔")
		}
		n, _ := strconv.Atoi(id)
		id = strconv.Itoa(n)
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { a, _ := strconv.Atoi(ids[i]); b, _ := strconv.Atoi(ids[j]); return a < b })
	return strings.Join(ids, ","), nil
}

func phoneCountryAllowed(configured, country string) bool {
	if !countryPattern.MatchString(country) {
		return false
	}
	if configured == "*" {
		return true
	}
	for _, id := range strings.Split(configured, ",") {
		if id == country {
			return true
		}
	}
	return false
}

func phonePriceAllowed(price, ceiling string) bool {
	p, ok := new(big.Rat).SetString(price)
	if !ok || p.Sign() <= 0 {
		return false
	}
	m, ok := new(big.Rat).SetString(ceiling)
	return ok && m.Sign() > 0 && p.Cmp(m) <= 0
}

func phoneCatalogRequest(s Settings) (provider.Request, error) {
	countries, err := normalizePhoneCountries(s.PhoneCountry)
	if err != nil {
		return provider.Request{}, err
	}
	if countries == "" || !servicePattern.MatchString(s.PhoneService) || !pricePattern.MatchString(s.PhoneMaxPrice) || !phonePriceAllowed(s.PhoneMaxPrice, s.PhoneMaxPrice) {
		return provider.Request{}, errors.New("请先设置手机号国家和价格上限")
	}
	if countries == "*" {
		countries = ""
	}
	return provider.Request{Kind: "phone", Service: s.PhoneService, Country: countries, MaxPrice: s.PhoneMaxPrice, TTL: time.Duration(s.PhoneTTLMinutes) * time.Minute}, nil
}

func (a *App) readPhoneChannels(ctx context.Context, s Settings, fresh bool) ([]provider.PhoneChannel, error) {
	req, err := phoneCatalogRequest(s)
	if err != nil {
		return nil, err
	}
	client, ok := a.currentClient().(provider.PhoneCatalogClient)
	if !ok {
		return nil, errors.New("请先配置支持渠道查询的 API 密钥")
	}
	key := [4]string{hash(a.currentAPIKey()), req.Service, req.Country, req.MaxPrice}
	// Credential changes are excluded by providerGate, held by each caller.
	// Do not hold the cache mutex over network I/O: a stock rejection must be
	// visible immediately, including while a different catalog is loading.
	a.phoneCatalogMu.Lock()
	if !fresh {
		if cached, ok := a.phoneCatalogCache[key]; ok && time.Now().Before(cached.ExpiresAt) {
			channels := a.availablePhoneChannels(cached.Channels, key[0], req.Service)
			a.phoneCatalogMu.Unlock()
			return channels, nil
		}
	}
	a.phoneCatalogMu.Unlock()
	rows, err := client.PhoneChannels(ctx, req)
	if err != nil {
		return nil, errors.New(safeError(err))
	}
	countries, _ := normalizePhoneCountries(s.PhoneCountry)
	channels := []provider.PhoneChannel{}
	for _, ch := range rows {
		if !phoneCountryAllowed(countries, ch.Country) || ch.Count <= 0 || !phonePriceAllowed(ch.Price, req.MaxPrice) {
			continue
		}
		channels = append(channels, ch)
	}
	a.phoneCatalogMu.Lock()
	defer a.phoneCatalogMu.Unlock()
	if len(a.phoneCatalogCache) >= 32 {
		a.phoneCatalogCache = nil
	}
	if a.phoneCatalogCache == nil {
		a.phoneCatalogCache = make(map[[4]string]phoneCatalogCache)
	}
	a.phoneCatalogCache[key] = phoneCatalogCache{Channels: channels, ExpiresAt: time.Now().Add(20 * time.Second)}
	return a.availablePhoneChannels(channels, key[0], req.Service), nil
}

func (a *App) phoneCatalogResponse(w http.ResponseWriter, r *http.Request, s Settings) {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	channels, err := a.readPhoneChannels(ctx, s, false)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	respond(w, 200, map[string]any{"kind": "phone", "channels": channels, "max_price": s.PhoneMaxPrice, "countries": s.PhoneCountry, "resume": false})
}

func (a *App) adminPhoneCountries(w http.ResponseWriter, r *http.Request) {
	a.providerGate.RLock()
	defer a.providerGate.RUnlock()
	client, ok := a.currentClient().(provider.PhoneCatalogClient)
	if !ok {
		fail(w, 409, "请先保存 API 密钥")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	countries, err := client.PhoneCountries(ctx)
	if err != nil {
		fail(w, 502, safeError(err))
		return
	}
	respond(w, 200, map[string]any{"countries": countries})
}

func (a *App) adminPhoneChannels(w http.ResponseWriter, r *http.Request) {
	a.providerGate.RLock()
	defer a.providerGate.RUnlock()
	s := a.currentSettings()
	q := r.URL.Query()
	if q.Has("service") {
		s.PhoneService = q.Get("service")
	}
	if q.Has("countries") {
		s.PhoneCountry = q.Get("countries")
	}
	if q.Has("max_price") {
		s.PhoneMaxPrice = q.Get("max_price")
	}
	a.phoneCatalogResponse(w, r, s)
}

func (a *App) voucherPhoneChannels(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CDK string `json:"cdk"`
	}
	if !decode(w, r, &in) {
		return
	}
	code := normalizeCDK(in.CDK)
	if len(code) < 5 || len(code) > 100 {
		fail(w, 400, "请输入有效兑换码")
		return
	}
	c, err := scanCDK(a.db.QueryRow("SELECT "+cdkColumns+" FROM cdks WHERE hash=?", hash(code)))
	if err == sql.ErrNoRows {
		fail(w, 404, "兑换码不存在")
		return
	}
	if err != nil {
		a.databaseError(w, err)
		return
	}
	a.voucherCatalog(w, r, c)
}

func (a *App) orderPhoneChannels(w http.ResponseWriter, r *http.Request) {
	o, err := a.tokenOrder(r)
	if err != nil {
		fail(w, 401, err.Error())
		return
	}
	c, err := a.getCDK(o.CDKID)
	if err != nil {
		a.databaseError(w, err)
		return
	}
	a.voucherCatalog(w, r, c)
}

func (a *App) voucherCatalog(w http.ResponseWriter, r *http.Request, c CDK) {
	a.providerGate.RLock()
	defer a.providerGate.RUnlock()
	unlock := a.lock(c.ID)
	defer unlock()
	c, err := a.getCDK(c.ID)
	if err != nil {
		a.databaseError(w, err)
		return
	}
	if c.Status == "disabled" {
		fail(w, 409, "兑换码已停用")
		return
	}
	previous, err := a.latestOrder(c.ID)
	if err != nil && err != sql.ErrNoRows {
		a.databaseError(w, err)
		return
	}
	resume := err == nil && (c.Status != "available" || !terminalOrder(previous.Status) || time.Now().After(c.ExpiresAt))
	if resume {
		respond(w, 200, map[string]any{"kind": c.Kind, "channels": []provider.PhoneChannel{}, "resume": true})
		return
	}
	if time.Now().After(c.ExpiresAt) {
		fail(w, 410, "兑换码已过期")
		return
	}
	if c.Status != "available" || c.UsedCount >= c.UsageLimit {
		fail(w, 409, "兑换次数已用完")
		return
	}
	if err := a.validateReady(c); err != nil {
		fail(w, 409, err.Error())
		return
	}
	if c.Kind != "phone" {
		respond(w, 200, map[string]any{"kind": c.Kind, "channels": []provider.PhoneChannel{}, "resume": false})
		return
	}
	a.phoneCatalogResponse(w, r, a.effectivePhoneSettings(c.Snapshot))
}

// Resolve a user selection using server-owned voucher/admin settings. Never
// accept a submitted price, service or a country outside the saved allowlist.
func (a *App) selectedPhoneRequest(s Settings, choice phoneSelection) (provider.Request, *provider.PhoneChannel, error) {
	if _, ok := a.currentClient().(provider.PhoneCatalogClient); !ok && choice.Country == "" && choice.ProviderID == "" && a.opts.Client != nil {
		return provider.Request{Kind: "phone", Service: s.PhoneService, Country: s.PhoneCountry, MaxPrice: s.PhoneMaxPrice, TTL: time.Duration(s.PhoneTTLMinutes) * time.Minute}, nil, nil
	}
	allowed, err := normalizePhoneCountries(s.PhoneCountry)
	if err != nil {
		return provider.Request{}, nil, err
	}
	if !phoneCountryAllowed(allowed, choice.Country) || choice.ProviderID == "" || len(choice.ProviderID) > 20 {
		return provider.Request{}, nil, errors.New("请选择可用的手机号渠道")
	}
	if _, err := strconv.ParseUint(choice.ProviderID, 10, 64); err != nil {
		return provider.Request{}, nil, errors.New("渠道编号无效")
	}
	if a.isPhoneUnavailable(s.PhoneService, choice) {
		return provider.Request{}, nil, errors.New(phoneUnavailableMessage)
	}
	s.PhoneCountry = choice.Country
	ctx, cancel := a.providerContext()
	defer cancel()
	channels, err := a.readPhoneChannels(ctx, s, true)
	if err != nil {
		return provider.Request{}, nil, err
	}
	for _, ch := range channels {
		if ch.Country == choice.Country && ch.ProviderID == choice.ProviderID {
			req, err := phoneCatalogRequest(s)
			req.ProviderID = ch.ProviderID
			// A price jump after this lookup must fail rather than silently buy
			// the same channel at the administrator's potentially higher ceiling.
			req.MaxPrice = ch.Price
			return req, &ch, err
		}
	}
	if a.isPhoneUnavailable(s.PhoneService, choice) {
		return provider.Request{}, nil, errors.New(phoneUnavailableMessage)
	}
	return provider.Request{}, nil, errors.New("该渠道已售罄或价格变化，请刷新后重新选择")
}
