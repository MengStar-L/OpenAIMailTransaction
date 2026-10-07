//go:build !windows

package updater

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func executableSuffix() string                { return "" }
func canReplaceRunning() bool                 { return true }
func configureDetached(command *exec.Cmd)     { command.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
func replaceFile(source, target string) error { return os.Rename(source, target) }
func waitForProcess(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("无法确认旧进程状态")
		}
		// A detached helper can briefly observe the exited parent's zombie.
		if data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
			end := strings.LastIndexByte(string(data), ')')
			if end >= 0 && strings.HasPrefix(string(data[end+1:]), " Z") {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("旧进程尚未退出")
}
