package app

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", a.publicConfig)
	mux.HandleFunc("GET /api/inventory", a.inventory)
	mux.HandleFunc("POST /api/redeem", a.redeem)
	mux.HandleFunc("GET /api/orders/current", a.currentOrder)
	mux.HandleFunc("POST /api/orders/cancel", a.cancelOrder)
	mux.HandleFunc("POST /api/orders/complete", a.completeOrder)
	mux.HandleFunc("POST /api/orders/next-code", a.nextCode)
	mux.HandleFunc("POST /api/orders/replace", a.replaceOrder)
	mux.HandleFunc("POST /api/orders/queue-wait", a.updateQueueWait)
	mux.HandleFunc("POST /api/admin/login", a.login)
	mux.HandleFunc("GET /api/admin/bootstrap", a.bootstrapStatus)
	mux.HandleFunc("POST /api/admin/setup", a.setupAdmin)
	mux.HandleFunc("GET /api/admin/session", a.admin(func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]bool{"authenticated": true}) }))
	mux.HandleFunc("POST /api/admin/logout", a.admin(a.logout))
	mux.HandleFunc("GET /api/admin/overview", a.admin(a.overview))
	mux.HandleFunc("GET /api/admin/resources", a.admin(a.listAdminResources))
	mux.HandleFunc("POST /api/admin/resources", a.admin(a.allocateAdminResource))
	mux.HandleFunc("GET /api/admin/resources/{id}", a.adminResourceAction(a.currentOrder))
	mux.HandleFunc("POST /api/admin/resources/{id}/cancel", a.adminResourceAction(a.cancelOrder))
	mux.HandleFunc("POST /api/admin/resources/{id}/complete", a.adminResourceAction(a.completeOrder))
	mux.HandleFunc("POST /api/admin/resources/{id}/next-code", a.adminResourceAction(a.nextCode))
	mux.HandleFunc("POST /api/admin/resources/{id}/queue-wait", a.adminResourceAction(a.updateQueueWait))
	mux.HandleFunc("GET /api/admin/cdks", a.admin(a.listCDKs))
	mux.HandleFunc("POST /api/admin/cdks", a.admin(a.createCDKs))
	mux.HandleFunc("GET /api/admin/exports/{token}", a.admin(a.exportCDKs))
	mux.HandleFunc("POST /api/admin/cdks/{id}/disable", a.admin(a.disableCDK))
	mux.HandleFunc("POST /api/admin/cdks/{id}/code", a.admin(a.restoreCDKCode))
	mux.HandleFunc("GET /api/admin/settings", a.admin(a.getSettings))
	mux.HandleFunc("PUT /api/admin/settings", a.admin(a.putSettings))
	mux.HandleFunc("GET /api/admin/updates", a.admin(a.updateStatus))
	mux.HandleFunc("PUT /api/admin/updates/settings", a.admin(a.updatePreferences))
	mux.HandleFunc("POST /api/admin/updates/check", a.admin(a.checkUpdate))
	mux.HandleFunc("POST /api/admin/updates/apply", a.admin(a.applyUpdate))
	mux.HandleFunc("POST /api/admin/orders/{id}/resolve", a.admin(a.resolveOrder))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method != "GET" && r.Method != "HEAD" {
			origin := r.Header.Get("Origin")
			if origin != "" {
				u, err := url.Parse(origin)
				if err != nil || (u.Scheme != "https" && u.Scheme != "http") || !strings.EqualFold(u.Host, r.Host) {
					fail(w, 403, "请求来源无效")
					return
				}
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				fail(w, 403, "请求来源无效")
				return
			}
		}
		ip := a.clientIP(r)
		limit, window, key := 300, time.Minute, "api:"+ip
		if r.URL.Path == "/api/admin/login" || r.URL.Path == "/api/admin/setup" {
			limit = 10
			window = 10 * time.Minute
			key = "login:" + ip
		} else if r.URL.Path == "/api/redeem" || r.URL.Path == "/api/orders/replace" {
			limit = 30
			key = "redeem:" + ip
		}
		if !a.allow(key, limit, window) {
			w.Header().Set("Retry-After", "60")
			fail(w, 429, "操作过于频繁，请稍后重试")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *App) clientIP(r *http.Request) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(peer)
	if err != nil {
		return peer
	}
	ip = ip.Unmap()
	trusted := func(addr netip.Addr) bool {
		for _, prefix := range a.trustedProxies {
			if prefix.Contains(addr) {
				return true
			}
		}
		return false
	}
	if !trusted(ip) {
		return ip.String()
	}
	chain := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	if len(chain) > 20 {
		return ip.String()
	}
	for i := len(chain) - 1; i >= 0; i-- {
		candidate, e := netip.ParseAddr(strings.TrimSpace(chain[i]))
		if e != nil {
			return ip.String()
		}
		candidate = candidate.Unmap()
		if !trusted(candidate) {
			return candidate.String()
		}
	}
	return ip.String()
}
func (a *App) allow(key string, limit int, window time.Duration) bool {
	a.rateMu.Lock()
	defer a.rateMu.Unlock()
	now := time.Now()
	if len(a.rates) > 1024 {
		for k, v := range a.rates {
			if now.After(v.Until) {
				delete(a.rates, k)
			}
		}
	}
	r, ok := a.rates[key]
	if !ok && len(a.rates) > 10000 {
		return false
	}
	if !ok || now.After(r.Until) {
		r = rateWindow{Until: now.Add(window)}
	}
	r.Count++
	a.rates[key] = r
	return r.Count <= limit
}
func (a *App) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("atelier_admin")
		if err != nil {
			fail(w, 401, "请先登录")
			return
		}
		v, ok := a.sessions.Load(hash(cookie.Value))
		if !ok {
			fail(w, 401, "登录已失效")
			return
		}
		if time.Now().After(v.(time.Time)) {
			a.sessions.Delete(hash(cookie.Value))
			fail(w, 401, "登录已过期")
			return
		}
		next(w, r)
	}
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	ready, err := a.adminInitialized()
	if err != nil {
		a.databaseError(w, err)
		return
	}
	if !ready {
		fail(w, 409, "请先设置管理员密码")
		return
	}
	valid, err := a.passwordOK(in.Password)
	if err != nil {
		a.databaseError(w, err)
		return
	}
	if !valid {
		fail(w, 401, "密码不正确")
		return
	}
	a.startAdminSession(w, r)
	respond(w, 200, map[string]bool{"ok": true})
}
func (a *App) startAdminSession(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	a.sessions.Range(func(k, v any) bool {
		if now.After(v.(time.Time)) {
			a.sessions.Delete(k)
		}
		return true
	})
	token := randomString(32)
	a.sessions.Store(hash(token), now.Add(8*time.Hour))
	http.SetCookie(w, &http.Cookie{Name: "atelier_admin", Value: token, Path: "/api/admin", HttpOnly: true, Secure: a.opts.SecureCookies || r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 8 * 60 * 60})
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie("atelier_admin"); e == nil {
		a.sessions.Delete(hash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: "atelier_admin", Value: "", Path: "/api/admin", HttpOnly: true, Secure: a.opts.SecureCookies || r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	respond(w, 200, map[string]bool{"ok": true})
}
func (a *App) publicConfig(w http.ResponseWriter, r *http.Request) {
	s := a.currentSettings()
	respond(w, 200, map[string]any{"brand": s.Brand, "mode": a.opts.Mode, "configured": a.providerReady(), "phone_enabled": s.PhoneEnabled, "email_enabled": s.EmailEnabled, "background_type": s.BackgroundType, "background_url": s.BackgroundURL, "queue_default_minutes": allocationQueueDefaultMinutes, "queue_min_minutes": allocationQueueMinMinutes, "queue_max_minutes": allocationQueueMaxMinutes, "server_time": time.Now().UTC()})
}
