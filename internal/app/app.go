package app

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gofrs/flock"
	"io"
	"log"
	"net/http"
	"net/netip"
	"openai-mail-transaction/internal/provider"
	"openai-mail-transaction/internal/updater"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Options struct {
	TrustedProxies                 string
	DataDir, Mode, APIKey, APIBase string
	SecureCookies                  bool
	Client                         provider.Client
	Logger                         *log.Logger
	DisableWorker                  bool
	Updates                        *updater.Manager
}
type App struct {
	updateApplying         atomic.Bool
	updateMaintenance      atomic.Bool
	trustedProxies         []netip.Prefix
	fileLock               *flock.Flock
	db                     *sql.DB
	opts                   Options
	client                 provider.Client
	providerKey            string
	clientMu               sync.RWMutex
	providerGate           sync.RWMutex
	inventoryMu            sync.Mutex
	inventoryCache         inventoryCache
	allocationObservations map[[4]string]allocationObservation
	phoneCatalogMu         sync.Mutex
	phoneCatalogCache      map[[4]string]phoneCatalogCache
	phoneUnavailable       map[[4]string]time.Time
	log                    *log.Logger
	secret                 []byte
	settings               Settings
	settingsMu             sync.RWMutex
	locks                  sync.Map
	sessions               sync.Map
	exports                sync.Map
	rateMu                 sync.Mutex
	rates                  map[string]rateWindow
	ctx                    context.Context
	cancel                 context.CancelFunc
	wg                     sync.WaitGroup
}
type rateWindow struct {
	Count int
	Until time.Time
}

func New(opts Options) (*App, error) {
	if opts.Mode != "demo" && opts.Mode != "live" {
		return nil, errors.New("APP_MODE must be demo or live")
	}
	if opts.APIBase == "" {
		opts.APIBase = "https://smsbower.page"
	}
	if opts.DataDir == "" {
		opts.DataDir = "data"
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if err := os.MkdirAll(opts.DataDir, 0700); err != nil {
		return nil, err
	}
	fileLock := flock.New(filepath.Join(opts.DataDir, "instance.lock"))
	locked, err := fileLock.TryLock()
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, errors.New("another process is using this DATA_DIR")
	}
	lockOwned := true
	defer func() {
		if lockOwned {
			_ = fileLock.Unlock()
		}
	}()
	db, err := openStore(opts.DataDir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{db: db, opts: opts, log: opts.Logger, rates: make(map[string]rateWindow), ctx: ctx, cancel: cancel}
	a.fileLock = fileLock
	ok := false
	defer func() {
		if !ok {
			cancel()
			db.Close()
		}
	}()
	for _, raw := range strings.Split(opts.TrustedProxies, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		prefix, e := netip.ParsePrefix(raw)
		if e != nil {
			return nil, fmt.Errorf("invalid TRUSTED_PROXIES CIDR: %s", raw)
		}
		a.trustedProxies = append(a.trustedProxies, prefix)
	}
	var storedMode string
	err = db.QueryRow("SELECT value FROM metadata WHERE key='mode'").Scan(&storedMode)
	if err == sql.ErrNoRows {
		_, err = db.Exec("INSERT INTO metadata(key,value) VALUES('mode',?)", opts.Mode)
	} else if err == nil && storedMode != opts.Mode {
		return nil, errors.New("data directory belongs to another mode; choose a separate DATA_DIR for live and demo")
	}
	if err != nil {
		return nil, err
	}
	secretPath := filepath.Join(opts.DataDir, "secret.key")
	a.secret, err = os.ReadFile(secretPath)
	if os.IsNotExist(err) {
		a.secret = make([]byte, 32)
		if _, err = rand.Read(a.secret); err != nil {
			return nil, err
		}
		err = os.WriteFile(secretPath, a.secret, 0600)
	}
	if err != nil {
		return nil, err
	}
	if len(a.secret) != 32 {
		return nil, errors.New("invalid data/secret.key")
	}
	if err = a.migrateAdmin(); err != nil {
		return nil, err
	}
	a.settings = defaultSettings()
	if opts.Mode == "demo" {
		a.settings.PhoneCountry, a.settings.PhoneMaxPrice = "*", "0.30"
	}
	var settingsJSON string
	err = db.QueryRow("SELECT value FROM settings WHERE id=1").Scan(&settingsJSON)
	if err == nil {
		err = json.Unmarshal([]byte(settingsJSON), &a.settings)
	} else if err == sql.ErrNoRows {
		b, _ := json.Marshal(a.settings)
		_, err = db.Exec("INSERT INTO settings(id,value) VALUES(1,?)", string(b))
	}
	if err != nil {
		return nil, err
	}
	if err = a.initProvider(); err != nil {
		return nil, err
	}
	if err = a.loadPhoneUnavailable(); err != nil {
		return nil, err
	}
	if err = a.recoverOrders(); err != nil {
		return nil, err
	}
	if opts.Mode == "demo" {
		if err = a.seedDemo(); err != nil {
			return nil, err
		}
	}
	if !opts.DisableWorker {
		a.wg.Add(2)
		go a.worker()
		go a.allocationQueueWorker()
	}
	ok = true
	lockOwned = false
	return a, nil
}
func (a *App) Close() error {
	a.cancel()
	a.wg.Wait()
	err := a.db.Close()
	if a.fileLock != nil {
		_ = a.fileLock.Unlock()
	}
	return err
}
func (a *App) lock(id string) func() {
	v, _ := a.locks.LoadOrStore(id, &sync.Mutex{})
	m := v.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}
func (a *App) currentSettings() Settings {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	return a.settings
}
func hash(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
}
func normalizeCDK(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }
func (a *App) orderToken(id string) string {
	m := hmac.New(sha256.New, a.secret)
	m.Write([]byte("order:" + id))
	return id + "." + hex.EncodeToString(m.Sum(nil))
}
func (a *App) tokenOrder(r *http.Request) (Order, error) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return Order{}, errors.New("请重新输入兑换码")
	}
	token := strings.TrimPrefix(h, "Bearer ")
	id, _, ok := strings.Cut(token, ".")
	if !ok || len(token) > 200 || subtle.ConstantTimeCompare([]byte(token), []byte(a.orderToken(id))) != 1 {
		return Order{}, errors.New("兑换会话已失效")
	}
	o, err := a.getOrder(id)
	if err != nil {
		return o, errors.New("兑换记录不存在")
	}
	if o.CDKID == "" {
		return Order{}, errors.New("兑换会话已失效")
	}
	return o, nil
}
func (a *App) providerContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(a.ctx, 25*time.Second)
}
func (a *App) seedDemo() error {
	b, _ := json.Marshal(a.currentSettings())
	now := time.Now().UTC()
	for _, kind := range []string{"phone", "email"} {
		code := "DEMO-" + strings.ToUpper(kind)
		encrypted, err := a.encryptCDKCode(code)
		if err != nil {
			return err
		}
		_, err = a.db.Exec(`INSERT OR IGNORE INTO cdks(id,hash,masked_code,batch_id,kind,status,note,created_at,expires_at,attempts,max_attempts,snapshot,code_cipher,usage_limit) VALUES(?,?,?,?,?,'available',?,?,?,0,5,?,?,1)`, randomString(10), hash(code), code, "DEMO", kind, "演示兑换码", now.UnixMilli(), now.AddDate(1, 0, 0).UnixMilli(), string(b), encrypted)
		if err != nil {
			return err
		}
		if _, err = a.db.Exec("UPDATE cdks SET code_cipher=? WHERE hash=? AND code_cipher=''", encrypted, hash(code)); err != nil {
			return err
		}
	}
	return nil
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, 415, "请使用 JSON 请求")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		fail(w, 400, "请求内容无效")
		return false
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		fail(w, 400, "请求内容无效")
		return false
	}
	return true
}
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, msg string) {
	respond(w, status, map[string]string{"error": msg})
}
func (a *App) databaseError(w http.ResponseWriter, err error) {
	a.log.Printf("database operation failed: %v", err)
	fail(w, 500, "保存失败，请稍后重试")
}
func (a *App) orderResponse(w http.ResponseWriter, o Order, withToken bool) {
	a.decorate(&o)
	v := map[string]any{"order": o, "server_time": time.Now().UTC()}
	if withToken && o.CDKID != "" {
		v["token"] = a.orderToken(o.ID)
	}
	respond(w, 200, v)
}
func safeError(err error) string {
	var e *provider.Error
	if errors.As(err, &e) && e.Message != "" {
		return e.Message
	}
	return "上游暂时无法连接，请稍后重试"
}
func (a *App) transitionError(o *Order, err error) {
	a.log.Printf("order %s transition persistence failed: %v", o.ID, err)
}
func (a *App) validateReady(c CDK) error {
	if !a.providerReady() {
		return errors.New("请先在后台设置中保存 SMSBower API 密钥")
	}
	s := a.currentSettings()
	if c.Kind == "phone" && !s.PhoneEnabled || c.Kind == "email" && !s.EmailEnabled {
		return fmt.Errorf("该资源已暂停兑换")
	}
	if a.opts.Mode == "live" {
		v := c.Snapshot
		if c.Kind == "phone" {
			v = a.effectivePhoneSettings(v)
		}
		if c.Kind == "phone" && (v.PhoneMaxPrice == "" || v.PhoneCountry == "") {
			return errors.New("请联系管理员配置手机号国家和价格上限")
		}
		if c.Kind == "email" && v.EmailMaxPrice == "" {
			return errors.New("请联系管理员配置邮箱价格上限")
		}
		if c.Kind == "email" && v.EmailDomain == "" {
			return errors.New("请联系管理员配置邮箱域名")
		}
	}
	return nil
}

func (a *App) providerReady() bool { return a.currentClient() != nil }

func (a *App) ProviderReady() bool { return a.providerReady() }
