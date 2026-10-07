package app

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func (a *App) overview(w http.ResponseWriter, r *http.Request) {
	stats := map[string]int{}
	var total, available, active, completed, review int
	err := a.db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(status='available' AND expires_at>?),0),COALESCE(SUM(status='active'),0),COALESCE(SUM(status='used'),0),COALESCE(SUM(status='review'),0) FROM cdks`, time.Now().UnixMilli()).Scan(&total, &available, &active, &completed, &review)
	if err != nil {
		a.databaseError(w, err)
		return
	}
	stats["total"] = total
	stats["available"] = available
	stats["active"] = active
	stats["completed"] = completed
	stats["review"] = review
	// Read and close rows before fetching related vouchers: SQLite intentionally
	// uses one writer connection to keep transition ordering predictable.
	rows, err := a.db.Query("SELECT " + orderColumns + " FROM orders ORDER BY created_at DESC,rowid DESC LIMIT 50")
	if err != nil {
		a.databaseError(w, err)
		return
	}
	orders := []AdminOrder{}
	for rows.Next() {
		o, e := scanOrder(rows)
		if e != nil {
			rows.Close()
			a.databaseError(w, e)
			return
		}
		orders = append(orders, AdminOrder{Order: o, ProviderID: o.ProviderID})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		a.databaseError(w, err)
		return
	}
	for i := range orders {
		a.decorateQueue(&orders[i].Order)
		c, e := a.getCDK(orders[i].CDKID)
		if e == nil {
			if e = a.hydrateCDKCode(&c); e != nil {
				a.databaseError(w, e)
				return
			}
			orders[i].CDKCode = c.Code
			orders[i].MaskedCode = c.MaskedCode
			orders[i].Note = c.Note
			orders[i].UsedCount, orders[i].UsageLimit = c.UsedCount, c.UsageLimit
		}
	}
	respond(w, 200, map[string]any{"stats": stats, "orders": orders})
}
func (a *App) listCDKs(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	if len(query) > 200 {
		fail(w, 400, "搜索内容过长")
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	if page > 1000000 {
		page = 1000000
	}
	where := " WHERE 1=1"
	args := []any{}
	if query != "" {
		where += " AND (instr(masked_code,?)>0 OR instr(note,?)>0 OR instr(batch_id,?)>0 OR hash=?)"
		args = append(args, query, query, query, hash(normalizeCDK(query)))
	}
	if status := r.URL.Query().Get("status"); status != "" {
		where += " AND status=?"
		args = append(args, status)
	}
	var total int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM cdks"+where, args...).Scan(&total); err != nil {
		a.databaseError(w, err)
		return
	}
	args = append(args, 50, (page-1)*50)
	rows, err := a.db.Query("SELECT "+cdkColumns+" FROM cdks"+where+" ORDER BY created_at DESC,rowid DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		a.databaseError(w, err)
		return
	}
	defer rows.Close()
	items := []CDK{}
	for rows.Next() {
		c, e := scanCDK(rows)
		if e != nil {
			a.databaseError(w, e)
			return
		}
		if e = a.hydrateCDKCode(&c); e != nil {
			a.databaseError(w, e)
			return
		}
		items = append(items, c)
	}
	if err = rows.Err(); err != nil {
		a.databaseError(w, err)
		return
	}
	respond(w, 200, map[string]any{"items": items, "total": total, "page": page, "page_size": 50})
}
func (a *App) createCDKs(w http.ResponseWriter, r *http.Request) {
	a.providerGate.RLock()
	defer a.providerGate.RUnlock()
	var in struct {
		Kind        string `json:"kind"`
		Quantity    int    `json:"quantity"`
		Note        string `json:"note"`
		ExpiresDays int    `json:"expires_days"`
		MaxAttempts int    `json:"max_attempts"`
		UsageLimit  int    `json:"usage_limit"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.UsageLimit == 0 {
		in.UsageLimit = in.MaxAttempts // Compatibility for old clients during an upgrade.
	}
	if (in.Kind != "phone" && in.Kind != "email") || in.Quantity < 1 || in.Quantity > 200 || in.ExpiresDays < 1 || in.ExpiresDays > 365 || in.UsageLimit < 1 || in.UsageLimit > 1000 || len(in.Note) > 500 {
		fail(w, 400, "请检查类型、数量、有效期和可用次数")
		return
	}
	s := a.currentSettings()
	if err := a.validateReady(CDK{Kind: in.Kind, Snapshot: s}); err != nil {
		fail(w, 409, err.Error())
		return
	}
	snapshot, _ := json.Marshal(s)
	batchID := "B-" + time.Now().UTC().Format("20060102") + "-" + randomString(5)
	now := time.Now().UTC()
	expires := now.Add(time.Duration(in.ExpiresDays) * 24 * time.Hour)
	tx, err := a.db.Begin()
	if err != nil {
		a.databaseError(w, err)
		return
	}
	defer tx.Rollback()
	codes := make([]string, 0, in.Quantity)
	for i := 0; i < in.Quantity; i++ {
		raw := randomString(15)
		prefix := "PH"
		if in.Kind == "email" {
			prefix = "EM"
		}
		code := prefix + "-" + raw[:6] + "-" + raw[6:12] + "-" + raw[12:18] + "-" + raw[18:]
		masked := code[:9] + "-••••-" + code[len(code)-6:]
		encrypted, encryptionErr := a.encryptCDKCode(code)
		if encryptionErr != nil {
			fail(w, 500, "兑换码保存失败，请重试")
			return
		}
		_, err = tx.Exec(`INSERT INTO cdks(id,hash,masked_code,batch_id,kind,status,note,created_at,expires_at,attempts,max_attempts,snapshot,code_cipher,usage_limit) VALUES(?,?,?,?,?,'available',?,?,?,0,?,?,?,?)`, randomString(10), hash(code), masked, batchID, in.Kind, strings.TrimSpace(in.Note), now.UnixMilli(), expires.UnixMilli(), in.UsageLimit, string(snapshot), encrypted, in.UsageLimit)
		if err != nil {
			a.databaseError(w, err)
			return
		}
		codes = append(codes, code)
	}
	if err = tx.Commit(); err != nil {
		a.databaseError(w, err)
		return
	}
	a.audit("create_cdks", batchID, fmt.Sprintf("kind=%s quantity=%d", in.Kind, in.Quantity))
	exportToken := randomString(24)
	a.exports.Range(func(k, v any) bool {
		if now.After(v.(codeExport).ExpiresAt) {
			a.exports.Delete(k)
		}
		return true
	})
	a.exports.Store(exportToken, codeExport{Codes: codes, BatchID: batchID, ExpiresAt: now.Add(10 * time.Minute)})
	respond(w, 200, map[string]any{"codes": codes, "batch_id": batchID, "export_url": "/api/admin/exports/" + exportToken})
}

type codeExport struct {
	Codes     []string
	BatchID   string
	ExpiresAt time.Time
}

// Exports are available for ten minutes and expired material is swept in memory.
// No plaintext voucher is
// written to the database, a query string, or request logs. Authentication is
// required in addition to the random, unguessable download token.
func (a *App) exportCDKs(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	value, ok := a.exports.Load(token)
	if !ok {
		fail(w, 404, "导出已失效，请使用生成时保存的兑换码")
		return
	}
	item := value.(codeExport)
	if time.Now().After(item.ExpiresAt) {
		a.exports.Delete(token)
		fail(w, 410, "导出链接已过期，请使用生成时保存的兑换码")
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="CDK-%s.csv"`, item.BatchID))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte{0xef, 0xbb, 0xbf})
	writer := csv.NewWriter(w)
	writer.UseCRLF = true
	_ = writer.Write([]string{"CDK", "批次"})
	for _, code := range item.Codes {
		_ = writer.Write([]string{code, item.BatchID})
	}
	writer.Flush()
}
func (a *App) disableCDK(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	unlock := a.lock(id)
	defer unlock()
	res, err := a.db.Exec("UPDATE cdks SET status='disabled' WHERE id=? AND status='available'", id)
	if err != nil {
		a.databaseError(w, err)
		return
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		fail(w, 409, "只有未使用的兑换码可以停用")
		return
	}
	a.audit("disable_cdk", id, "")
	respond(w, 200, map[string]bool{"ok": true})
}

func (a *App) restoreCDKCode(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &in) {
		return
	}
	code := normalizeCDK(in.Code)
	if len(code) < 5 || len(code) > 100 {
		fail(w, 400, "兑换码格式无效")
		return
	}
	id := r.PathValue("id")
	unlock := a.lock(id)
	defer unlock()
	c, err := a.getCDK(id)
	if err == sql.ErrNoRows {
		fail(w, 404, "兑换码不存在")
		return
	}
	if err != nil {
		a.databaseError(w, err)
		return
	}
	if c.Hash != hash(code) {
		fail(w, 400, "完整码与该记录不匹配")
		return
	}
	if err = a.rememberCDKCode(id, code); err != nil {
		a.databaseError(w, err)
		return
	}
	a.audit("restore_cdk_code", id, "")
	respond(w, 200, map[string]bool{"ok": true})
}
func (a *App) getSettings(w http.ResponseWriter, r *http.Request) {
	respond(w, 200, a.settingsResponse(a.currentSettings()))
}

var servicePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)
var countryPattern = regexp.MustCompile(`^[0-9]{1,5}$`)
var pricePattern = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,4})(\.[0-9]{1,6})?$`)
var domainPattern = regexp.MustCompile(`^[a-zA-Z0-9.-]{1,128}$`)

func validateSettings(s Settings) error {
	if strings.TrimSpace(s.Brand) == "" || len([]rune(s.Brand)) > 24 {
		return errors.New("站点名称限 1–24 个字")
	}
	if !servicePattern.MatchString(s.PhoneService) || !servicePattern.MatchString(s.EmailService) {
		return errors.New("服务代码无效")
	}
	if _, err := normalizePhoneCountries(s.PhoneCountry); err != nil {
		return err
	}
	if s.EmailDomain != "" && !domainPattern.MatchString(s.EmailDomain) {
		return errors.New("邮箱域名无效")
	}
	for _, v := range []string{s.PhoneMaxPrice, s.EmailMaxPrice} {
		if v != "" {
			n, err := strconv.ParseFloat(v, 64)
			if !pricePattern.MatchString(v) || err != nil || n <= 0 {
				return errors.New("价格上限必须为正数")
			}
		}
	}
	if s.PhoneTTLMinutes < 3 || s.PhoneTTLMinutes > 60 || s.EmailTTLMinutes < 1 || s.EmailTTLMinutes > 60 {
		return errors.New("手机号等待时间为 3–60 分钟，邮箱为 1–60 分钟")
	}
	if s.BackgroundType != "none" && s.BackgroundType != "image" && s.BackgroundType != "video" {
		return errors.New("背景类型无效")
	}
	if len(s.BackgroundURL) > 2048 {
		return errors.New("背景地址过长")
	}
	if s.BackgroundURL != "" {
		u, err := url.Parse(s.BackgroundURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
			return errors.New("背景请使用 HTTPS 图片或视频地址")
		}
	}
	if s.BackgroundType != "none" && s.BackgroundURL == "" {
		return errors.New("请填写背景地址")
	}
	return nil
}
func (a *App) putSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Settings
		APIKey string `json:"api_key"`
	}
	if !decode(w, r, &in) {
		return
	}
	s := in.Settings
	normalizedCountries, err := normalizePhoneCountries(s.PhoneCountry)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.PhoneCountry = normalizedCountries
	if err := validateSettings(s); err != nil {
		fail(w, 400, err.Error())
		return
	}
	key := strings.TrimSpace(in.APIKey)
	if key != "" {
		if err := validateAPIKey(key); err != nil {
			fail(w, 400, err.Error())
			return
		}
	}
	// Orders hold the read side through their durable transition. A credential
	// change cannot overtake an allocation or switch accounts during a poll.
	a.providerGate.Lock()
	defer a.providerGate.Unlock()
	oldKey := a.currentAPIKey()
	changed := key != "" && key != oldKey
	if changed && oldKey != "" {
		var unsettled int
		err := a.db.QueryRow("SELECT COUNT(*) FROM orders WHERE status IN ('queued','allocating','waiting','received','cancel_pending','review','next_pending','next_uncertain','complete_pending')").Scan(&unsettled)
		if err != nil {
			a.databaseError(w, err)
			return
		}
		if unsettled > 0 {
			fail(w, 409, "请先完成或核对进行中的订单，再更换 API 密钥")
			return
		}
	}
	client := a.currentClient()
	var encrypted string
	if changed {
		client, err = a.configuredClient(key)
		if err != nil {
			fail(w, 400, "服务连接配置无效")
			return
		}
		encrypted, err = a.encryptAPIKey(key)
		if err != nil {
			fail(w, 500, "密钥保存失败，请稍后重试")
			return
		}
	}
	b, _ := json.Marshal(s)
	tx, err := a.db.Begin()
	if err != nil {
		a.databaseError(w, err)
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE settings SET value=? WHERE id=1", string(b)); err == nil && changed {
		_, err = tx.Exec("INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", providerKeyMetadata, encrypted)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		a.databaseError(w, err)
		return
	}
	a.settingsMu.Lock()
	a.settings = s
	a.settingsMu.Unlock()
	if changed {
		a.clientMu.Lock()
		a.client, a.providerKey = client, key
		a.clientMu.Unlock()
	}
	a.audit("update_settings", "1", "")
	respond(w, 200, a.settingsResponse(s))
}
func (a *App) resolveOrder(w http.ResponseWriter, r *http.Request) {
	a.providerGate.RLock()
	defer a.providerGate.RUnlock()
	var in struct {
		Resolution string `json:"resolution"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Resolution != "released" && in.Resolution != "consumed" {
		fail(w, 400, "请选择核对结果")
		return
	}
	o, err := a.getOrder(r.PathValue("id"))
	if err == sql.ErrNoRows {
		fail(w, 404, "订单不存在")
		return
	}
	if err != nil {
		a.databaseError(w, err)
		return
	}
	unlock := a.lock(orderLockKey(o))
	defer unlock()
	o, err = a.getOrder(o.ID)
	if err != nil {
		a.databaseError(w, err)
		return
	}
	if o.Status != "review" && o.Status != "next_uncertain" {
		fail(w, 409, "该订单无需人工核对")
		return
	}
	if in.Resolution == "released" {
		if o.UsageCounted || len(o.Codes) > 0 || o.Code != "" {
			fail(w, 409, "已收到验证码的订单不能恢复兑换额度")
			return
		}
		o.Status = "cancelled"
		o.Message = "管理员已确认资源释放"
		err = a.release(&o)
	} else {
		o.Status = "completed"
		o.Message = "管理员已确认使用"
		o.UsageCounted = true
		err = a.release(&o)
	}
	if err != nil {
		a.databaseError(w, err)
		return
	}
	a.audit("resolve_order", o.ID, in.Resolution)
	respond(w, 200, map[string]bool{"ok": true})
}
