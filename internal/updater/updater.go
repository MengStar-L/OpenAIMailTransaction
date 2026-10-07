// Package updater retrieves stable releases from the repository compiled into
// the application. It never changes application settings, credentials or data.
package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"
)

var ErrBusy = errors.New("正在检查或安装更新")

type Settings struct {
	AutoCheck  bool `json:"auto_check"`
	AutoUpdate bool `json:"auto_update"`
}

type State struct {
	CurrentVersion  string    `json:"current_version"`
	LatestVersion   string    `json:"latest_version,omitempty"`
	Repository      string    `json:"repository"`
	ReleaseURL      string    `json:"release_url,omitempty"`
	Available       bool      `json:"available"`
	Supported       bool      `json:"supported"`
	Phase           string    `json:"phase"`
	DownloadedBytes int64     `json:"downloaded_bytes"`
	TotalBytes      int64     `json:"total_bytes"`
	Error           string    `json:"error,omitempty"`
	LastChecked     time.Time `json:"last_checked,omitempty"`
	Settings        Settings  `json:"settings"`
}

type Options struct {
	Version    string
	Repository string
	DataDir    string
	// Shutdown must request graceful shutdown without blocking. The caller should
	// close its database and return from main; never terminate in this callback.
	Shutdown func()
	// BeforeApply may defer updates while orders or billing actions are active.
	// It is checked before download and again immediately before installation.
	BeforeApply func() error
	// CancelApply releases any maintenance gate acquired by BeforeApply when
	// installation is deferred or fails. It is not called after a successful
	// installation because the process is about to shut down.
	CancelApply func()
	// systemd requires Restart=always. An empty value detects INVOCATION_ID;
	// otherwise process uses a detached helper to restart the program.
	RestartMode string
	// HTTPClient is optional, primarily for deterministic tests. The normal
	// client restricts all redirects to GitHub release asset hosts.
	HTTPClient *http.Client
	Executable string
	Interval   time.Duration
}

type Manager struct {
	mu           sync.Mutex
	state        State
	options      Options
	client       *http.Client
	settingsPath string
	busy         bool
	start        sync.Once
	wake         chan struct{}
	install      func(context.Context, string, string) error
}

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9_.-]+$`)

func New(options Options) (*Manager, error) {
	if !repositoryPattern.MatchString(options.Repository) {
		return nil, fmt.Errorf("更新仓库未配置")
	}
	if options.Version == "" {
		options.Version = "dev"
	}
	if options.DataDir == "" {
		return nil, fmt.Errorf("更新数据目录未配置")
	}
	if options.Interval == 0 {
		options.Interval = 6 * time.Hour
	}
	if options.Interval < time.Minute {
		return nil, fmt.Errorf("更新检查间隔太短")
	}
	if options.Executable == "" {
		var err error
		options.Executable, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("无法定位程序文件")
		}
	}
	options.Executable, _ = filepath.Abs(options.Executable)
	if resolved, err := filepath.EvalSymlinks(options.Executable); err == nil {
		options.Executable = resolved
	}
	if options.RestartMode == "" {
		options.RestartMode = "process"
		if runtime.GOOS == "linux" && os.Getenv("INVOCATION_ID") != "" {
			options.RestartMode = "systemd"
		}
	}
	if options.RestartMode != "systemd" && options.RestartMode != "process" {
		return nil, fmt.Errorf("更新重启方式无效")
	}
	if err := os.MkdirAll(options.DataDir, 0700); err != nil {
		return nil, fmt.Errorf("无法创建更新设置目录")
	}
	client := options.HTTPClient
	if client == nil {
		client = githubClient()
	}
	m := &Manager{options: options, client: client, settingsPath: filepath.Join(options.DataDir, "update-settings.json"), wake: make(chan struct{}, 1)}
	_, versionErr := parseVersion(options.Version)
	m.state = State{CurrentVersion: options.Version, Repository: options.Repository, Phase: "idle", Supported: versionErr == nil && options.Shutdown != nil && (runtime.GOOS == "linux" || runtime.GOOS == "windows"), Settings: Settings{AutoCheck: true}}
	if data, err := os.ReadFile(m.settingsPath); err == nil {
		if len(data) > 4096 || json.Unmarshal(data, &m.state.Settings) != nil {
			return nil, fmt.Errorf("更新设置文件损坏")
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("无法读取更新设置")
	}
	if m.state.Settings.AutoUpdate {
		m.state.Settings.AutoCheck = true
	}
	m.install = m.installCandidate
	return m, nil
}

func (m *Manager) State() State       { m.mu.Lock(); defer m.mu.Unlock(); return m.state }
func (m *Manager) Settings() Settings { return m.State().Settings }

func (m *Manager) SetSettings(settings Settings) error {
	if settings.AutoUpdate {
		settings.AutoCheck = true
	}
	m.mu.Lock()
	if settings.AutoUpdate && !m.state.Supported {
		m.mu.Unlock()
		return fmt.Errorf("当前程序版本不支持自动安装")
	}
	body, _ := json.MarshalIndent(settings, "", "  ")
	if err := writeAtomic(m.settingsPath, body, 0600); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("无法保存更新设置")
	}
	m.state.Settings = settings
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return nil
}

// Start runs one background loop until ctx is cancelled. The first check is
// delayed to keep release lookups off the service startup path.
func (m *Manager) Start(ctx context.Context) {
	m.start.Do(func() {
		go func() {
			timer := time.NewTimer(15 * time.Second)
			defer timer.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
				case <-m.wake:
				}
				settings := m.Settings()
				if settings.AutoCheck {
					checkCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
					state, err := m.Check(checkCtx)
					cancel()
					if err == nil && state.Available && m.Settings().AutoUpdate {
						applyCtx, cancel := context.WithTimeout(ctx, 8*time.Minute)
						_, _ = m.Apply(applyCtx)
						cancel()
					}
				}
				interval := m.options.Interval
				if m.State().Phase == "deferred" && interval > 15*time.Minute {
					interval = 15 * time.Minute
				}
				timer.Reset(interval)
			}
		}()
	})
}

func (m *Manager) begin(phase string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy {
		return ErrBusy
	}
	m.busy = true
	m.state.Phase = phase
	m.state.Error = ""
	m.state.DownloadedBytes = 0
	m.state.TotalBytes = 0
	return nil
}

func (m *Manager) fail(err error) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busy = false
	m.state.Phase = "error"
	m.state.Error = err.Error()
	return m.state, err
}

func (m *Manager) recordRelease(release releaseInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.LastChecked = time.Now().UTC()
	m.state.LatestVersion = release.Tag
	m.state.ReleaseURL = "https://github.com/" + m.options.Repository + "/releases/tag/" + release.Tag
	m.state.Available = newer(release.Tag, m.options.Version)
}

func (m *Manager) Check(ctx context.Context) (State, error) {
	if err := m.begin("checking"); err != nil {
		return m.State(), err
	}
	release, err := m.latest(ctx)
	if err != nil {
		m.mu.Lock()
		m.state.LastChecked = time.Now().UTC()
		m.mu.Unlock()
		return m.fail(err)
	}
	m.recordRelease(release)
	m.mu.Lock()
	m.busy = false
	m.state.Phase = "idle"
	state := m.state
	m.mu.Unlock()
	return state, nil
}

func (m *Manager) Apply(ctx context.Context) (State, error) {
	if err := m.begin("downloading"); err != nil {
		return m.State(), err
	}
	committed := false
	gateAttempted := false
	defer func() {
		if !committed {
			if gateAttempted && m.options.CancelApply != nil {
				m.options.CancelApply()
			}
			// Release the operation lock only after clearing the maintenance
			// gate, otherwise a concurrent Apply could lose its new gate.
			m.mu.Lock()
			m.busy = false
			m.mu.Unlock()
		}
	}()
	if !m.State().Supported {
		return m.applyFail(fmt.Errorf("当前程序版本不支持安装更新"))
	}
	gateAttempted = m.options.BeforeApply != nil
	if err := m.beforeApply(); err != nil {
		return m.deferApply(err)
	}
	// Re-read the official release instead of accepting any client-supplied
	// version, archive URL or checksums.
	release, err := m.latest(ctx)
	if err != nil {
		return m.applyFail(err)
	}
	m.recordRelease(release)
	if !newer(release.Tag, m.options.Version) {
		return m.applyFail(fmt.Errorf("当前已是最新正式版本"))
	}
	dir, err := os.MkdirTemp(filepath.Dir(m.options.Executable), ".shiguang-update-")
	if err != nil {
		return m.applyFail(fmt.Errorf("程序目录不可写，无法安装更新"))
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()
	archive, checksum, err := m.downloadRelease(ctx, release, dir)
	if err != nil {
		return m.applyFail(err)
	}
	candidate, err := extractBinary(archive, dir, runtime.GOOS)
	if err != nil {
		return m.applyFail(err)
	}
	_ = checksum // archive checksum was verified before any extraction.
	digest, err := fileSHA256(candidate)
	if err != nil {
		return m.applyFail(fmt.Errorf("无法校验程序文件"))
	}
	if err = m.beforeApply(); err != nil {
		return m.deferApply(err)
	}
	if err = m.install(ctx, candidate, digest); err != nil {
		return m.applyFail(err)
	}
	keep = m.options.RestartMode != "systemd"
	committed = true
	m.mu.Lock()
	m.state.Phase = "restarting"
	state := m.state
	m.mu.Unlock()
	// Keep busy=true until this process exits, making repeated Apply idempotent.
	go func() { time.Sleep(750 * time.Millisecond); m.options.Shutdown() }()
	return state, nil
}

func (m *Manager) beforeApply() error {
	if m.options.BeforeApply != nil {
		return m.options.BeforeApply()
	}
	return nil
}

func (m *Manager) deferApply(err error) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.Phase = "deferred"
	m.state.Error = err.Error()
	return m.state, err
}

func (m *Manager) applyFail(err error) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.Phase = "error"
	m.state.Error = err.Error()
	return m.state, err
}
