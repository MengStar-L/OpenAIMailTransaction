package main

import (
	"bufio"
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"openai-mail-transaction/internal/app"
	"openai-mail-transaction/internal/updater"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

//go:embed web/*
var web embed.FS

var (
	version    = "dev"
	commit     = "unknown"
	buildDate  = "unknown"
	repository = "MengStar-L/OpenAIMailTransaction"
)

func main() {
	if handled, err := updater.RunHelper(os.Args[1:]); handled {
		if err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Printf("shiguang %s (%s, %s)\n", version, commit, buildDate)
		return
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	if err := loadEnv(".env"); err != nil {
		return err
	}
	mode := env("APP_MODE", "demo")
	addr := env("LISTEN_ADDR", "0.0.0.0:8080")
	dataDir := env("DATA_DIR", filepath.Join("data", mode))
	updateShutdown := make(chan struct{}, 1)
	var application *app.App
	updates, err := updater.New(updater.Options{Version: version, Repository: repository, DataDir: dataDir, RestartMode: env("UPDATE_RESTART_MODE", ""), Shutdown: func() {
		select {
		case updateShutdown <- struct{}{}:
		default:
		}
	}, BeforeApply: func() error {
		if application == nil {
			return errors.New("服务尚未准备好")
		}
		return application.CanUpdate()
	}, CancelApply: func() {
		if application != nil {
			application.CancelUpdate()
		}
	}})
	if err != nil {
		return err
	}
	application, err = app.New(app.Options{DataDir: dataDir, Mode: mode, APIKey: os.Getenv("SMSBOWER_API_KEY"), APIBase: env("SMSBOWER_API_BASE", "https://smsbower.page"), SecureCookies: os.Getenv("COOKIE_SECURE") == "true", TrustedProxies: os.Getenv("TRUSTED_PROXIES"), Updates: updates})
	if err != nil {
		return err
	}
	defer application.Close()
	updateContext, stopUpdates := context.WithCancel(context.Background())
	defer stopUpdates()
	updates.Start(updateContext)
	files, err := fs.Sub(web, "web")
	if err != nil {
		return err
	}
	static := http.FileServer(http.FS(files))
	mux := http.NewServeMux()
	mux.Handle("/api/", application.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		if r.URL.Path == "/" || r.URL.Path == "/admin" || r.URL.Path == "/admin/" {
			body, e := fs.ReadFile(files, "index.html")
			if e != nil {
				http.Error(w, "Unavailable", 500)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(body)
			return
		}
		if r.URL.Path != "/styles.css" && r.URL.Path != "/app.js" &&
			r.URL.Path != "/mail-alerts.css" && r.URL.Path != "/mail-alerts.js" &&
			r.URL.Path != "/system-updates.js" && r.URL.Path != "/system-updates.css" &&
			r.URL.Path != "/admin-balance.js" && r.URL.Path != "/admin-balance.css" &&
			r.URL.Path != "/selects.js" && r.URL.Path != "/selects.css" &&
			r.URL.Path != "/phone-channels.js" && r.URL.Path != "/phone-channels.css" &&
			r.URL.Path != "/assets/ZCOOLKuaiLe-Regular.woff2" &&
			r.URL.Path != "/assets/ZCOOLKuaiLe-Regular.ttf" && r.URL.Path != "/assets/ZCOOLKuaiLe-OFL.txt" &&
			r.URL.Path != "/assets/Nunito-Variable.ttf" && r.URL.Path != "/assets/Nunito-OFL.txt" &&
			r.URL.Path != "/assets/NotoSansSC-Variable.woff2" && r.URL.Path != "/assets/NotoSansSC-OFL.txt" &&
			r.URL.Path != "/assets/shiguang-mascot.png" && r.URL.Path != "/assets/shiguang-icon-128.png" &&
			r.URL.Path != "/assets/shiguang-favicon-32.png" && r.URL.Path != "/assets/shiguang-icon-192.png" &&
			r.URL.Path != "/assets/shiguang-apple-180.png" {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=86400")
		}
		static.ServeHTTP(w, r)
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; media-src 'self' https:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		mux.ServeHTTP(w, r)
	})
	server := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	errorsCh := make(chan error, 1)
	go func() { errorsCh <- server.ListenAndServe() }()
	log.Printf("拾光已启动 · %s · http://%s", mode, addr)
	log.Printf("管理后台: http://%s/admin", addr)
	if mode == "demo" {
		log.Print("演示兑换码: DEMO-PHONE / DEMO-EMAIL（不会购买真实资源）")
	} else if !application.ProviderReady() {
		log.Print("正式模式已启动；请在后台设置中保存 API 密钥后使用资源")
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case err = <-errorsCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-signals:
	case <-updateShutdown:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	return server.Shutdown(ctx)
}
func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// Environment variables override the optional local .env file. Values are data,
// never shell-expanded or executed.
func loadEnv(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !ok || key == "" || strings.ContainsAny(key, " \t\r\n") {
			return fmt.Errorf("invalid .env line %d", line)
		}
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err = os.Setenv(key, value); err != nil {
				return fmt.Errorf("invalid .env key on line %d", line)
			}
		}
	}
	return scanner.Err()
}
