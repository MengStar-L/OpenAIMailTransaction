package updater

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// A response with no Content-Length that pauses partway through its body lets
// the test observe progress while Apply is still running.
type pausedDownloadBody struct {
	prefix *bytes.Reader
	suffix *bytes.Reader
	paused chan struct{}
	resume chan struct{}
	ctx    context.Context
	once   sync.Once
}

func (b *pausedDownloadBody) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	b.once.Do(func() { close(b.paused) })
	select {
	case <-b.resume:
		return b.suffix.Read(p)
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
}

func (b *pausedDownloadBody) Close() error { return nil }

func TestApplyReportsIntermediateChunkedDownloadProgress(t *testing.T) {
	m := testManager(t)
	archive := makeArchive(t, runtime.GOOS, []archiveEntry{{"shiguang" + executableSuffix(), bytes.Repeat([]byte("new program contents"), 4096), false}})
	mockRelease(m, "v1.1.0", archive, false)
	original := m.client.Transport
	paused, resume := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unpause := func() { releaseOnce.Do(func() { close(resume) }) }
	defer unpause()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, expectedAssetName("v1.1.0", runtime.GOOS, runtime.GOARCH)) {
			return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Header: make(http.Header), Body: &pausedDownloadBody{
				prefix: bytes.NewReader(archive[:len(archive)/2]), suffix: bytes.NewReader(archive[len(archive)/2:]), paused: paused, resume: resume, ctx: r.Context(),
			}}, nil
		}
		return original.RoundTrip(r)
	})
	installed := make(chan State, 1)
	m.install = func(context.Context, string, string) error {
		installed <- m.State()
		return nil
	}
	type result struct {
		state State
		err   error
	}
	finished := make(chan result, 1)
	go func() { state, err := m.Apply(ctx); finished <- result{state, err} }()
	select {
	case <-paused:
	case <-ctx.Done():
		t.Fatal("download did not reach its pause")
	}
	state := m.State()
	if state.Phase != "downloading" || state.DownloadedBytes != int64(len(archive)/2) || state.TotalBytes != int64(len(archive)) {
		t.Fatalf("progress not visible during download: %+v", state)
	}
	if _, err := m.Check(ctx); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent check did not stay busy: %v", err)
	}
	if got := m.State(); got.DownloadedBytes != state.DownloadedBytes || got.TotalBytes != state.TotalBytes {
		t.Fatalf("busy operation reset active download: %+v", got)
	}
	unpause()
	select {
	case got := <-finished:
		if got.err != nil || got.state.Phase != "restarting" || got.state.DownloadedBytes != int64(len(archive)) || got.state.TotalBytes != int64(len(archive)) {
			t.Fatalf("completed download state: %+v, %v", got.state, got.err)
		}
	case <-ctx.Done():
		t.Fatal("update did not complete")
	}
	state = <-installed
	if state.Phase != "verifying" || state.DownloadedBytes != int64(len(archive)) || state.TotalBytes != int64(len(archive)) {
		t.Fatalf("installation ran before verification phase: %+v", state)
	}
}

func TestDownloadFailureProgressResetsOnNextOperation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		badSHA    bool
		shortBody bool
		readError bool
		wantError string
	}{
		{name: "checksum", badSHA: true, wantError: "SHA256"},
		{name: "size", shortBody: true, wantError: "大小不一致"},
		{name: "connection", readError: true, wantError: "下载"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testManager(t)
			archive := []byte("test archive")
			mockRelease(m, "v1.1.0", archive, tc.badSHA)
			original := m.client.Transport
			wantDownloaded := len(archive)
			if tc.shortBody || tc.readError {
				wantDownloaded = len(archive) / 2
				m.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, expectedAssetName("v1.1.0", runtime.GOOS, runtime.GOARCH)) {
						body := io.Reader(bytes.NewReader(archive[:wantDownloaded]))
						if tc.readError {
							body = io.MultiReader(body, brokenDownloadReader{})
						}
						return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Header: make(http.Header), Body: io.NopCloser(body)}, nil
					}
					return original.RoundTrip(r)
				})
			}
			m.install = func(context.Context, string, string) error {
				t.Error("invalid download reached installation")
				return nil
			}
			state, err := m.Apply(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.wantError) || state.Phase != "error" || state.DownloadedBytes != int64(wantDownloaded) || state.TotalBytes != int64(len(archive)) {
				t.Fatalf("wrong failed download state: %+v, %v", state, err)
			}
			state, err = m.Check(context.Background())
			if err != nil || state.Phase != "idle" || state.DownloadedBytes != 0 || state.TotalBytes != 0 || state.Error != "" {
				t.Fatalf("check retained previous transfer progress: %+v, %v", state, err)
			}
			// A new attempt deferred before downloading must also discard the
			// previous transfer; otherwise the UI briefly claims it is complete.
			m.mu.Lock()
			m.state.DownloadedBytes, m.state.TotalBytes = 20, 20
			m.mu.Unlock()
			m.options.BeforeApply = func() error { return errors.New("active orders") }
			state, err = m.Apply(context.Background())
			if err == nil || state.Phase != "deferred" || state.DownloadedBytes != 0 || state.TotalBytes != 0 {
				t.Fatalf("deferred update retained previous transfer: %+v, %v", state, err)
			}
		})
	}
}

type brokenDownloadReader struct{}

func (brokenDownloadReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type partialDownloadWriter struct{}

func (partialDownloadWriter) Write(p []byte) (int, error) { return len(p) / 2, io.ErrShortWrite }

func TestDownloadProgressCountsOnlyAcknowledgedWrites(t *testing.T) {
	m := testManager(t)
	w := &downloadProgressWriter{writer: partialDownloadWriter{}, manager: m}
	n, err := w.Write([]byte("12345678"))
	if n != 4 || !errors.Is(err, io.ErrShortWrite) || m.State().DownloadedBytes != 4 {
		t.Fatalf("reported bytes that were not written: n=%d err=%v state=%+v", n, err, m.State())
	}
}
