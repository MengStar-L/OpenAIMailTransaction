package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const helperFlag = "--self-update-helper"

type helperManifest struct {
	ParentPID        int      `json:"parent_pid"`
	Executable       string   `json:"executable"`
	Candidate        string   `json:"candidate"`
	Digest           string   `json:"sha256"`
	Directory        string   `json:"directory"`
	Args             []string `json:"args"`
	LogPath          string   `json:"log_path"`
	AlreadyInstalled bool     `json:"already_installed"`
}

func (m *Manager) installCandidate(ctx context.Context, candidate, digest string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("更新已取消")
	}
	if m.options.RestartMode == "systemd" {
		return installBinary(m.options.Executable, candidate, digest)
	}
	workingDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("无法保存程序工作目录")
	}
	dir := filepath.Dir(candidate)
	helper := filepath.Join(dir, "shiguang-update-helper"+executableSuffix())
	if err = copyFile(m.options.Executable, helper, 0700); err != nil {
		return fmt.Errorf("无法准备更新重启程序")
	}
	manifest := helperManifest{ParentPID: os.Getpid(), Executable: m.options.Executable, Candidate: candidate, Digest: digest, Directory: workingDir, Args: os.Args[1:], LogPath: filepath.Join(m.options.DataDir, "update.log")}
	manifest.LogPath, _ = filepath.Abs(manifest.LogPath)
	// Linux supports replacing the executable while the old process runs.
	// Windows keeps it locked until the parent has fully exited.
	if canReplaceRunning() {
		if err = installBinary(manifest.Executable, candidate, digest); err != nil {
			return err
		}
		manifest.AlreadyInstalled = true
	}
	manifestPath := filepath.Join(dir, "restart.json")
	body, _ := json.Marshal(manifest)
	if err = writeAtomic(manifestPath, body, 0600); err != nil {
		if manifest.AlreadyInstalled {
			_ = restorePrevious(manifest.Executable)
		}
		return fmt.Errorf("无法保存更新重启信息")
	}
	output, err := openUpdateLog(manifest.LogPath)
	if err != nil {
		if manifest.AlreadyInstalled {
			_ = restorePrevious(manifest.Executable)
		}
		return fmt.Errorf("无法打开更新日志")
	}
	defer output.Close()
	command := exec.Command(helper, helperFlag, manifestPath)
	command.Dir = workingDir
	command.Stdout = output
	command.Stderr = output
	configureDetached(command)
	if err = command.Start(); err != nil {
		if manifest.AlreadyInstalled {
			_ = restorePrevious(manifest.Executable)
		}
		return fmt.Errorf("无法启动更新重启程序")
	}
	_ = command.Process.Release()
	return nil
}

func openUpdateLog(path string) (*os.File, error) {
	if info, err := os.Stat(path); err == nil && info.Size() > 1<<20 {
		_ = replaceFile(path, path+".previous")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
}

// RunHelper must be called at the very beginning of main, before loading .env
// or opening the database. Regular program arguments return handled=false.
// Helper inputs are local files created with private permissions; they are
// never exposed by the admin HTTP endpoints.
func RunHelper(args []string) (handled bool, err error) {
	if len(args) == 0 || args[0] != helperFlag {
		return false, nil
	}
	if len(args) != 2 {
		return true, fmt.Errorf("更新重启参数无效")
	}
	manifestPath, err := filepath.Abs(args[1])
	if err != nil {
		return true, fmt.Errorf("更新重启路径无效")
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil || len(data) > 64<<10 {
		return true, fmt.Errorf("无法读取更新重启信息")
	}
	var manifest helperManifest
	if json.Unmarshal(data, &manifest) != nil {
		return true, fmt.Errorf("更新重启信息无效")
	}
	self, err := os.Executable()
	if err != nil {
		return true, fmt.Errorf("无法定位更新程序")
	}
	if err = validateManifest(manifest, manifestPath, self); err != nil {
		return true, err
	}
	output, err := openUpdateLog(manifest.LogPath)
	if err != nil {
		return true, fmt.Errorf("无法打开更新日志")
	}
	defer output.Close()
	logger := log.New(output, "updater: ", log.LstdFlags|log.LUTC)
	err = runHelper(manifest, logger)
	if err != nil {
		logger.Print(err)
	} else {
		logger.Print("更新进程已启动")
	}
	// The running helper cannot delete itself on Windows. Small staging files
	// remain beside the binary for diagnosis; application data is never touched.
	if err == nil {
		_ = os.Remove(manifestPath)
		_ = os.Remove(manifest.Candidate)
		_ = os.Remove(self)
		_ = os.Remove(filepath.Dir(manifestPath))
	}
	return true, err
}

func validateManifest(m helperManifest, manifestPath, self string) error {
	dir := filepath.Dir(manifestPath)
	if m.ParentPID <= 1 || m.ParentPID == os.Getpid() || !filepath.IsAbs(m.Executable) || !filepath.IsAbs(m.Candidate) || !filepath.IsAbs(m.Directory) || !filepath.IsAbs(m.LogPath) {
		return fmt.Errorf("更新重启信息无效")
	}
	if filepath.Clean(filepath.Dir(self)) != dir || filepath.Clean(filepath.Dir(m.Candidate)) != dir || filepath.Clean(filepath.Dir(m.Executable)) != filepath.Dir(dir) || !strings.HasPrefix(filepath.Base(dir), ".shiguang-update-") {
		return fmt.Errorf("更新文件不在预期目录")
	}
	if len(m.Digest) != 64 {
		return fmt.Errorf("更新校验值无效")
	}
	return nil
}

type helperHooks struct {
	wait     func(int, time.Duration) error
	install  func(string, string, string) error
	launch   func(helperManifest) error
	rollback func(string) error
}

func runHelper(m helperManifest, logger *log.Logger) error {
	return runHelperWithHooks(m, logger, helperHooks{wait: waitForProcess, install: installBinary, launch: launchUpdated, rollback: restorePrevious})
}

func runHelperWithHooks(m helperManifest, logger *log.Logger, hooks helperHooks) error {
	if err := hooks.wait(m.ParentPID, 70*time.Second); err != nil {
		return fmt.Errorf("等待旧程序关闭超时，请查看服务状态")
	}
	if !m.AlreadyInstalled {
		if err := hooks.install(m.Executable, m.Candidate, m.Digest); err != nil {
			logger.Print(err)
			if restartErr := hooks.launch(m); restartErr != nil {
				return fmt.Errorf("更新安装失败，旧版文件仍保留，请手动启动")
			}
			return fmt.Errorf("更新安装失败，已重新启动旧版")
		}
	} else {
		digest, err := fileSHA256(m.Executable)
		if err != nil || digest != m.Digest {
			return fmt.Errorf("重启前程序校验失败")
		}
	}
	if err := hooks.launch(m); err != nil {
		logger.Print("新版程序启动失败，恢复旧版")
		if restoreErr := hooks.rollback(m.Executable); restoreErr != nil {
			return fmt.Errorf("新版启动失败，自动回退失败，请使用 .previous 文件恢复")
		}
		if fallbackErr := hooks.launch(m); fallbackErr != nil {
			return fmt.Errorf("已恢复旧版，自动启动失败，请手动启动")
		}
		return fmt.Errorf("新版无法启动，已自动恢复并启动旧版")
	}
	return nil
}

func launchUpdated(m helperManifest) error {
	output, err := openUpdateLog(m.LogPath)
	if err != nil {
		return err
	}
	defer output.Close()
	command := exec.Command(m.Executable, m.Args...)
	command.Dir = m.Directory
	command.Stdout = output
	command.Stderr = output
	configureDetached(command)
	if err = command.Start(); err != nil {
		return err
	}
	// Catch an immediately crashing executable as well as a failed exec. This
	// is a startup check, not a substitute for a monitored HTTP health check.
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	select {
	case <-exited:
		return fmt.Errorf("程序启动后立即退出")
	case <-time.After(3 * time.Second):
		return nil
	}
}

func restorePrevious(executable string) error {
	file, err := os.CreateTemp(filepath.Dir(executable), ".shiguang-rollback-*")
	if err != nil {
		return err
	}
	name := file.Name()
	file.Close()
	os.Remove(name)
	defer os.Remove(name)
	if err = copyFile(executable+".previous", name, 0755); err != nil {
		return err
	}
	return replaceFile(name, executable)
}
