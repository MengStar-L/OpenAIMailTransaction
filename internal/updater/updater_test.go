package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "shiguang"+executableSuffix())
	if err := os.WriteFile(exe, []byte("original binary"), 0755); err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{Version: "v1.0.0", Repository: "example/OpenAIMailTransaction", DataDir: filepath.Join(dir, "data"), Executable: exe, Shutdown: func() {}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestStableVersions(t *testing.T) {
	for _, tc := range []struct {
		candidate, current string
		want               bool
	}{
		{"v1.0.1", "v1.0.0", true}, {"1.10.0", "1.9.9", true}, {"v2.0.0", "1.99.99", true},
		{"v1.0.0", "v1.0.0", false}, {"1.0.0", "1.1.0", false}, {"v1.0.0", "dev", false},
		{"v1.1.0-beta.1", "1.0.0", false}, {"v1.1.0", "1.0.0-dev", false}, {"v01.1.0", "1.0.0", false},
		{"1.0.1+build.7", "1.0.0", true}, {"1.0.0+build.7", "1.0.0", false}, {"9999999999999999999999.0.0", "1.0.0", false},
	} {
		t.Run(tc.candidate+"_"+tc.current, func(t *testing.T) {
			if got := newer(tc.candidate, tc.current); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestSettingsPersistAndDevProtection(t *testing.T) {
	m := testManager(t)
	if m.Settings() != (Settings{AutoCheck: true}) {
		t.Fatal("wrong defaults")
	}
	if err := m.SetSettings(Settings{AutoUpdate: true}); err != nil {
		t.Fatal(err)
	}
	loaded, err := New(m.options)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Settings() != (Settings{AutoCheck: true, AutoUpdate: true}) {
		t.Fatal("settings not persisted")
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(m.settingsPath)
		if info.Mode().Perm() != 0600 {
			t.Fatal("settings not private")
		}
	}
	options := m.options
	options.Version = "dev"
	dev, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if dev.State().Supported {
		t.Fatal("dev install supported")
	}
	if err = dev.SetSettings(Settings{AutoUpdate: true}); err == nil {
		t.Fatal("dev automatic update enabled")
	}
	if _, err = dev.Apply(context.Background()); err == nil {
		t.Fatal("dev version overwritten")
	}
}

func response(body []byte) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Header: make(http.Header)}
}

func mockRelease(m *Manager, tag string, archive []byte, wrongChecksum bool) {
	name := expectedAssetName(tag, runtime.GOOS, runtime.GOARCH)
	base := "https://github.com/" + m.options.Repository + "/releases/download/" + tag + "/"
	digest := sha256.Sum256(archive)
	if wrongChecksum {
		digest = sha256.Sum256([]byte("wrong"))
	}
	checksums := []byte(hex.EncodeToString(digest[:]) + "  " + name + "\n")
	release, _ := json.Marshal(releaseInfo{Tag: tag, Assets: []releaseAsset{{Name: name, URL: base + name, Size: int64(len(archive))}, {Name: "checksums.txt", URL: base + "checksums.txt", Size: int64(len(checksums))}}})
	m.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case "https://api.github.com/repos/" + m.options.Repository + "/releases/latest":
			return response(release), nil
		case base + name:
			return response(archive), nil
		case base + "checksums.txt":
			return response(checksums), nil
		default:
			return nil, fmt.Errorf("unexpected address")
		}
	})}
}

func TestCheckAndConcurrency(t *testing.T) {
	m := testManager(t)
	mockRelease(m, "v1.2.0", []byte("package"), false)
	state, err := m.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !state.Available || state.LatestVersion != "v1.2.0" || state.LastChecked.IsZero() || state.Phase != "idle" {
		t.Fatalf("unexpected state: %+v", state)
	}
	entered := make(chan struct{})
	resume := make(chan struct{})
	var wg sync.WaitGroup
	m.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-resume
		return response([]byte(`{"tag_name":"v1.2.0"}`)), nil
	})}
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = m.Check(context.Background()) }()
	<-entered
	if _, err = m.Check(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("second check = %v", err)
	}
	if _, err = m.Apply(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent apply = %v", err)
	}
	close(resume)
	wg.Wait()
}

func TestMetadataAndSourcesRestricted(t *testing.T) {
	m := testManager(t)
	for _, body := range []string{`{"tag_name":"v1.2.0","prerelease":true}`, `{"tag_name":"v1.2.0","draft":true}`, `{"tag_name":"../../evil"}`, `{"tag_name":"dev"}`, `not json`} {
		m.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response([]byte(body)), nil })}
		if _, err := m.latest(context.Background()); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	for _, address := range []string{"https://evil.test/archive.zip", "https://github.com/other/repo/releases/download/v1.2.0/checksums.txt", "https://github.com/example/OpenAIMailTransaction/releases/download/v1.2.0/checksums.txt?x=1"} {
		_, err := m.findAsset(releaseInfo{Tag: "v1.2.0", Assets: []releaseAsset{{Name: "checksums.txt", URL: address}}}, "checksums.txt")
		if err == nil {
			t.Fatalf("accepted source %s", address)
		}
	}
	for _, address := range []string{"http://github.com/file", "https://github.com.evil.test/file", "https://github.com:8080/file", "https://user:pass@github.com/file", "https://127.0.0.1/file"} {
		u, _ := url.Parse(address)
		if trustedDownloadHost(u) {
			t.Fatalf("accepted redirect %s", address)
		}
	}
	u, _ := url.Parse("https://release-assets.githubusercontent.com/file?signature=test")
	if !trustedDownloadHost(u) {
		t.Fatal("GitHub redirect rejected")
	}
}

func TestDownloadSHAAndSize(t *testing.T) {
	m := testManager(t)
	archive := []byte("test archive")
	mockRelease(m, "v1.1.0", archive, false)
	release, err := m.latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	file, _, err := m.downloadRelease(context.Background(), release, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(file)
	if !bytes.Equal(body, archive) {
		t.Fatal("archive changed")
	}
	mockRelease(m, "v1.1.0", archive, true)
	release, _ = m.latest(context.Background())
	if _, _, err = m.downloadRelease(context.Background(), release, t.TempDir()); err == nil || !strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("bad SHA accepted: %v", err)
	}
	release.Assets[0].Size = maxArchiveSize + 1
	if _, _, err = m.downloadRelease(context.Background(), release, t.TempDir()); err == nil {
		t.Fatal("large archive accepted")
	}
	if _, err = checksumFor([]byte(strings.Repeat("a", 64)+"  pkg\n"+strings.Repeat("a", 64)+"  pkg\n"), "pkg"); err == nil {
		t.Fatal("duplicate SHA accepted")
	}
}

type archiveEntry struct {
	name string
	data []byte
	link bool
}

func makeArchive(t *testing.T, goos string, entries []archiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	if goos == "windows" {
		w := zip.NewWriter(&buf)
		for _, e := range entries {
			header := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
			header.SetMode(0755)
			if e.link {
				header.SetMode(os.ModeSymlink | 0755)
			}
			writer, err := w.CreateHeader(header)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = writer.Write(e.data); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		gz := gzip.NewWriter(&buf)
		w := tar.NewWriter(gz)
		for _, e := range entries {
			header := &tar.Header{Name: e.name, Mode: 0755, Size: int64(len(e.data)), Typeflag: tar.TypeReg}
			if e.link {
				header.Typeflag = tar.TypeSymlink
				header.Linkname = "/etc/passwd"
				header.Size = 0
			}
			if err := w.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if !e.link {
				if _, err := w.Write(e.data); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func TestArchivesRejectTraversalLinksDuplicates(t *testing.T) {
	for _, goos := range []string{"windows", "linux"} {
		t.Run(goos, func(t *testing.T) {
			name := "shiguang"
			if goos == "windows" {
				name += ".exe"
			}
			cases := []struct {
				name    string
				entries []archiveEntry
				want    bool
			}{
				{"valid", []archiveEntry{{name, []byte("binary"), false}, {"LICENSE", []byte("license"), false}}, true},
				{"parent", []archiveEntry{{"../evil", []byte("bad"), false}, {name, []byte("binary"), false}}, false},
				{"absolute", []archiveEntry{{"/tmp/evil", []byte("bad"), false}}, false},
				{"backslash", []archiveEntry{{`..\evil`, []byte("bad"), false}}, false},
				{"symlink", []archiveEntry{{name, nil, true}}, false},
				{"duplicate", []archiveEntry{{name, []byte("first"), false}, {name, []byte("second"), false}}, false},
				{"empty", []archiveEntry{{name, nil, false}}, false},
				{"missing", []archiveEntry{{"README.md", []byte("hi"), false}}, false},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					dir := t.TempDir()
					archive := filepath.Join(dir, "package")
					os.WriteFile(archive, makeArchive(t, goos, tc.entries), 0600)
					file, err := extractBinary(archive, dir, goos)
					if (err == nil) != tc.want {
						t.Fatalf("extract err = %v", err)
					}
					if tc.want {
						data, _ := os.ReadFile(file)
						if string(data) != "binary" {
							t.Fatal("wrong binary")
						}
					}
				})
			}
		})
	}
}

func TestInstallBacksUpAndRejectsTampering(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "shiguang")
	candidate := filepath.Join(dir, "candidate")
	os.WriteFile(target, []byte("old binary"), 0755)
	os.WriteFile(candidate, []byte("new binary"), 0755)
	digest, _ := fileSHA256(candidate)
	if err := installBinary(target, candidate, strings.Repeat("0", 64)); err == nil {
		t.Fatal("tampering accepted")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "old binary" {
		t.Fatal("modified current binary before verification")
	}
	if err := installBinary(target, candidate, digest); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(target)
	if string(data) != "new binary" {
		t.Fatal("new binary missing")
	}
	data, _ = os.ReadFile(target + ".previous")
	if string(data) != "old binary" {
		t.Fatal("backup missing")
	}
	if err := restorePrevious(target); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(target)
	if string(data) != "old binary" {
		t.Fatal("rollback failed")
	}
}

func TestApplyDefersActiveOrdersAndNeverInstallsBadPackage(t *testing.T) {
	m := testManager(t)
	m.options.BeforeApply = func() error { return errors.New("仍有进行中订单，请结束后更新") }
	state, err := m.Apply(context.Background())
	if err == nil || state.Phase != "deferred" {
		t.Fatalf("did not defer: %+v %v", state, err)
	}
	m.options.BeforeApply = nil
	mockRelease(m, "v1.1.0", []byte("bad package"), true)
	called := false
	m.install = func(context.Context, string, string) error { called = true; return nil }
	if _, err = m.Apply(context.Background()); err == nil {
		t.Fatal("bad package accepted")
	}
	if called {
		t.Fatal("installed unverified package")
	}
}

func TestApplyChecksActivityAgainAfterDownload(t *testing.T) {
	m := testManager(t)
	name := "shiguang" + executableSuffix()
	mockRelease(m, "v1.1.0", makeArchive(t, runtime.GOOS, []archiveEntry{{name, []byte("new binary"), false}}), false)
	checks := 0
	m.options.BeforeApply = func() error {
		checks++
		if checks == 2 {
			return errors.New("仍有进行中订单，请结束后更新")
		}
		return nil
	}
	called := false
	m.install = func(context.Context, string, string) error { called = true; return nil }
	state, err := m.Apply(context.Background())
	if err == nil || state.Phase != "deferred" || checks != 2 || called {
		t.Fatalf("unsafe activity race: %+v %v %d %v", state, err, checks, called)
	}
}

func TestHelperWaitsAndRollsBackFailedLaunch(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	var actions []string
	hooks := helperHooks{wait: func(int, time.Duration) error { actions = append(actions, "wait"); return nil }, install: func(string, string, string) error { actions = append(actions, "install"); return nil }, launch: func(helperManifest) error {
		actions = append(actions, "launch")
		if len(actions) == 3 {
			return errors.New("cannot execute")
		}
		return nil
	}, rollback: func(string) error { actions = append(actions, "rollback"); return nil }}
	if err := runHelperWithHooks(helperManifest{}, logger, hooks); err == nil || !strings.Contains(err.Error(), "已自动恢复") {
		t.Fatalf("rollback status = %v", err)
	}
	if strings.Join(actions, ",") != "wait,install,launch,rollback,launch" {
		t.Fatalf("wrong sequence %v", actions)
	}
	actions = nil
	hooks.wait = func(int, time.Duration) error { return errors.New("still alive") }
	if err := runHelperWithHooks(helperManifest{}, logger, hooks); err == nil {
		t.Fatal("live parent accepted")
	}
	if len(actions) != 0 {
		t.Fatal("changed binary before parent exit")
	}
	actions = nil
	hooks.wait = func(int, time.Duration) error { return nil }
	hooks.install = func(string, string, string) error { return errors.New("cannot replace") }
	hooks.launch = func(helperManifest) error { actions = append(actions, "restart-old"); return nil }
	if err := runHelperWithHooks(helperManifest{}, logger, hooks); err == nil || !strings.Contains(err.Error(), "重新启动旧版") {
		t.Fatalf("failed installation = %v", err)
	}
	if strings.Join(actions, ",") != "restart-old" {
		t.Fatalf("old program not restarted: %v", actions)
	}
}

func TestHelperRejectsUnexpectedPaths(t *testing.T) {
	dir := t.TempDir()
	stage := filepath.Join(dir, ".shiguang-update-test")
	m := helperManifest{ParentPID: 1234, Executable: filepath.Join(dir, "shiguang"), Candidate: filepath.Join(stage, "shiguang.new"), Directory: dir, LogPath: filepath.Join(dir, "data", "update.log"), Digest: strings.Repeat("a", 64)}
	manifest := filepath.Join(stage, "restart.json")
	self := filepath.Join(stage, "helper")
	if err := validateManifest(m, manifest, self); err != nil {
		t.Fatal(err)
	}
	m.Candidate = filepath.Join(dir, "outside")
	if err := validateManifest(m, manifest, self); err == nil {
		t.Fatal("outside candidate accepted")
	}
	if handled, err := RunHelper(nil); handled || err != nil {
		t.Fatal("normal start treated as helper")
	}
}

func TestNetworkErrorsDoNotExposeSignedURLs(t *testing.T) {
	m := testManager(t)
	m.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("https://example.test/?token=supersecret")
	})}
	_, err := m.Check(context.Background())
	if err == nil || strings.Contains(err.Error(), "supersecret") || strings.Contains(m.State().Error, "token") {
		t.Fatal("network details leaked")
	}
}

func TestApplyReleasesMaintenanceBeforeAllowingAnotherOperation(t *testing.T) {
	m := testManager(t)
	entered := false
	released := false
	m.options.BeforeApply = func() error { entered = true; return errors.New("active orders") }
	m.options.CancelApply = func() {
		if _, err := m.Check(context.Background()); !errors.Is(err, ErrBusy) {
			t.Errorf("operation unlocked before gate release: %v", err)
		}
		released = true
	}
	if _, err := m.Apply(context.Background()); err == nil {
		t.Fatal("deferred update succeeded")
	}
	if !entered || !released || m.busy {
		t.Fatalf("maintenance leak: entered=%v released=%v busy=%v", entered, released, m.busy)
	}
	released = false
	m.options.BeforeApply = func() error { return nil }
	mockRelease(m, "v1.1.0", []byte("bad SHA"), true)
	if _, err := m.Apply(context.Background()); err == nil {
		t.Fatal("invalid package installed")
	}
	if !released || m.busy {
		t.Fatal("download failure left maintenance enabled")
	}
}
