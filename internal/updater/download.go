package updater

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const maxArchiveSize int64 = 128 << 20
const maxBinarySize int64 = 192 << 20
const maxMetadataSize int64 = 2 << 20

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}
type releaseInfo struct {
	Tag        string         `json:"tag_name"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []releaseAsset `json:"assets"`
}

func githubClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: transport, Timeout: 5 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !trustedDownloadHost(req.URL) {
			return fmt.Errorf("不允许的更新下载跳转")
		}
		return nil
	}}
}

func trustedDownloadHost(u *url.URL) bool {
	if u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "github.com", "api.github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com":
		return true
	default:
		return false
	}
}

func (m *Manager) get(ctx context.Context, address string, limit int64) ([]byte, error) {
	response, err := m.request(ctx, address)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.ContentLength > limit {
		return nil, fmt.Errorf("更新响应超过大小限制")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("读取更新数据失败")
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("更新响应超过大小限制")
	}
	return data, nil
}

func (m *Manager) request(ctx context.Context, address string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, fmt.Errorf("更新地址无效")
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "Shiguang-Updater/"+m.options.Version)
	response, err := m.client.Do(request)
	// Never include net/url errors: redirects may contain signed download URLs.
	if err != nil {
		return nil, fmt.Errorf("无法连接 GitHub，请稍后重试")
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		switch response.StatusCode {
		case 404:
			return nil, fmt.Errorf("GitHub 尚未发布正式版本")
		case 403, 429:
			return nil, fmt.Errorf("GitHub 请求受限，请稍后重试")
		default:
			return nil, fmt.Errorf("GitHub 暂不可用（HTTP %d）", response.StatusCode)
		}
	}
	return response, nil
}

func (m *Manager) latest(ctx context.Context) (releaseInfo, error) {
	var release releaseInfo
	data, err := m.get(ctx, "https://api.github.com/repos/"+m.options.Repository+"/releases/latest", maxMetadataSize)
	if err != nil {
		return release, err
	}
	if json.Unmarshal(data, &release) != nil {
		return release, fmt.Errorf("GitHub 版本信息格式无效")
	}
	if _, err = parseVersion(release.Tag); err != nil || release.Draft || release.Prerelease {
		return release, fmt.Errorf("GitHub 未提供有效正式版本")
	}
	return release, nil
}

func expectedAssetName(tag, goos, goarch string) string {
	extension := ".tar.gz"
	if goos == "windows" {
		extension = ".zip"
	}
	return "shiguang_" + assetVersion(tag) + "_" + goos + "_" + goarch + extension
}

func (m *Manager) findAsset(release releaseInfo, name string) (releaseAsset, error) {
	expected := "https://github.com/" + m.options.Repository + "/releases/download/" + url.PathEscape(release.Tag) + "/" + url.PathEscape(name)
	var result releaseAsset
	found := false
	for _, asset := range release.Assets {
		if asset.Name != name {
			continue
		}
		if found || asset.URL != expected {
			return result, fmt.Errorf("更新附件来源无效")
		}
		result = asset
		found = true
	}
	if !found {
		return result, fmt.Errorf("正式版本缺少当前平台安装包或校验文件")
	}
	return result, nil
}

func checksumFor(data []byte, name string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	result := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return "", fmt.Errorf("更新校验文件格式无效")
		}
		if strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		if result != "" {
			return "", fmt.Errorf("更新校验记录重复")
		}
		decoded, err := hex.DecodeString(fields[0])
		if err != nil || len(decoded) != sha256.Size {
			return "", fmt.Errorf("更新校验值无效")
		}
		result = strings.ToLower(fields[0])
	}
	if scanner.Err() != nil || result == "" {
		return "", fmt.Errorf("找不到安装包的校验值")
	}
	return result, nil
}

func (m *Manager) downloadRelease(ctx context.Context, release releaseInfo, dir string) (string, string, error) {
	name := expectedAssetName(release.Tag, runtime.GOOS, runtime.GOARCH)
	archive, err := m.findAsset(release, name)
	if err != nil {
		return "", "", err
	}
	checksums, err := m.findAsset(release, "checksums.txt")
	if err != nil {
		return "", "", err
	}
	if archive.Size <= 0 || archive.Size > maxArchiveSize || checksums.Size <= 0 || checksums.Size > maxMetadataSize {
		return "", "", fmt.Errorf("更新附件大小无效")
	}
	data, err := m.get(ctx, checksums.URL, maxMetadataSize)
	if err != nil {
		return "", "", err
	}
	digest, err := checksumFor(data, name)
	if err != nil {
		return "", "", err
	}
	response, err := m.request(ctx, archive.URL)
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()
	if response.ContentLength > maxArchiveSize {
		return "", "", fmt.Errorf("安装包超过大小限制")
	}
	archivePath := filepath.Join(dir, name)
	file, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", "", fmt.Errorf("无法保存安装包")
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, maxArchiveSize+1))
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		return "", "", fmt.Errorf("下载安装包失败")
	}
	if n != archive.Size || n > maxArchiveSize {
		return "", "", fmt.Errorf("安装包大小不一致")
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return "", "", fmt.Errorf("安装包 SHA256 校验失败，未安装")
	}
	return archivePath, digest, nil
}

func safeArchiveName(name string) bool {
	return name != "" && !strings.Contains(name, "\\") && !strings.Contains(name, ":") && !strings.HasPrefix(name, "/") && path.Clean(name) == name && name != ".." && !strings.HasPrefix(name, "../")
}

func extractBinary(archivePath, dir, goos string) (string, error) {
	wanted := "shiguang"
	if goos == "windows" {
		wanted = "shiguang.exe"
	}
	candidate := filepath.Join(dir, wanted+".new")
	found := false
	entries := 0
	write := func(name string, size int64, reader io.Reader) error {
		entries++
		if entries > 128 || !safeArchiveName(name) {
			return fmt.Errorf("安装包路径无效")
		}
		if size < 0 || size > maxBinarySize {
			return fmt.Errorf("安装包内容超过大小限制")
		}
		if name != wanted {
			return nil
		}
		if found || size == 0 {
			return fmt.Errorf("安装包程序文件无效")
		}
		file, err := os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
		if err != nil {
			return fmt.Errorf("无法提取程序文件")
		}
		n, copyErr := io.Copy(file, io.LimitReader(reader, maxBinarySize+1))
		syncErr := file.Sync()
		closeErr := file.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil || n != size || n > maxBinarySize {
			return fmt.Errorf("程序文件提取失败")
		}
		found = true
		return nil
	}
	if goos == "windows" {
		archive, err := zip.OpenReader(archivePath)
		if err != nil {
			return "", fmt.Errorf("安装包格式无效")
		}
		defer archive.Close()
		for _, entry := range archive.File {
			if !entry.Mode().IsRegular() || entry.UncompressedSize64 > uint64(maxBinarySize) {
				return "", fmt.Errorf("安装包含不允许的文件类型")
			}
			reader, err := entry.Open()
			if err != nil {
				return "", fmt.Errorf("无法读取安装包")
			}
			err = write(entry.Name, int64(entry.UncompressedSize64), reader)
			reader.Close()
			if err != nil {
				return "", err
			}
		}
	} else {
		file, err := os.Open(archivePath)
		if err != nil {
			return "", fmt.Errorf("无法读取安装包")
		}
		defer file.Close()
		gz, err := gzip.NewReader(file)
		if err != nil {
			return "", fmt.Errorf("安装包格式无效")
		}
		defer gz.Close()
		reader := tar.NewReader(io.LimitReader(gz, 2*maxBinarySize))
		for {
			entry, err := reader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", fmt.Errorf("安装包内容损坏")
			}
			if entry.Typeflag != tar.TypeReg && entry.Typeflag != tar.TypeRegA {
				return "", fmt.Errorf("安装包含不允许的文件类型")
			}
			if err = write(entry.Name, entry.Size, reader); err != nil {
				return "", err
			}
		}
	}
	if !found {
		return "", fmt.Errorf("安装包中找不到程序文件")
	}
	return candidate, nil
}
