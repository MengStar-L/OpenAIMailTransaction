package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, io.LimitReader(file, maxBinarySize+1)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".update-settings-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	return replaceFile(name, path)
}

func copyFile(source, target string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, io.LimitReader(input, maxBinarySize+1))
	syncErr := output.Sync()
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func installBinary(target, candidate, digest string) error {
	if info, err := os.Lstat(candidate); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("更新程序文件无效")
	}
	actual, err := fileSHA256(candidate)
	if err != nil || actual != digest {
		return fmt.Errorf("更新程序文件校验失败")
	}
	if info, err := os.Lstat(target); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("当前程序文件无效")
	}
	backup, err := os.CreateTemp(filepath.Dir(target), ".shiguang-backup-*")
	if err != nil {
		return fmt.Errorf("无法准备旧版程序备份")
	}
	backupPath := backup.Name()
	backup.Close()
	os.Remove(backupPath)
	defer os.Remove(backupPath)
	if err = copyFile(target, backupPath, 0700); err != nil {
		return fmt.Errorf("无法备份旧版程序")
	}
	if err = replaceFile(backupPath, target+".previous"); err != nil {
		return fmt.Errorf("无法保存旧版程序备份")
	}
	if err = os.Chmod(candidate, 0755); err != nil {
		return fmt.Errorf("无法设置新版程序权限")
	}
	if err = replaceFile(candidate, target); err != nil {
		return fmt.Errorf("无法替换程序文件，旧版仍可运行")
	}
	return nil
}
