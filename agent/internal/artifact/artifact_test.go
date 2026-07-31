package artifact

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTarGz(t *testing.T, path string, entries map[string]struct {
	data string
	mode int64
}) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, e := range entries {
		hdr := &tar.Header{Name: name, Size: int64(len(e.data)), Mode: e.mode}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadSetsAuthHeaderAndWritesFile(t *testing.T) {
	content := []byte("package-bytes")
	var gotBearer, gotJobToken, gotBasicUser, gotBasicPass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBearer = r.Header.Get("Authorization")
		gotJobToken = r.Header.Get("JOB-TOKEN")
		gotBasicUser, gotBasicPass, _ = r.BasicAuth()
		w.Write(content)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "pkg.tar.gz")
	err := Download(context.Background(), srv.Client(), srv.URL,
		&Auth{Type: "bearer", Token: "tok123"}, dest)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if gotBearer != "Bearer tok123" {
		t.Errorf("Authorization = %q", gotBearer)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(content) {
		t.Errorf("content mismatch: %q", got)
	}

	if err := Download(context.Background(), srv.Client(), srv.URL,
		&Auth{Type: "job_token", Token: "jt"}, dest); err != nil {
		t.Fatalf("download job_token: %v", err)
	}
	if gotJobToken != "jt" {
		t.Errorf("JOB-TOKEN = %q", gotJobToken)
	}

	// basic(Deploy Token,原则 5):HTTP Basic 认证头
	if err := Download(context.Background(), srv.Client(), srv.URL,
		&Auth{Type: "basic", Username: "deploy-user", Token: "dt-secret"}, dest); err != nil {
		t.Fatalf("download basic: %v", err)
	}
	if gotBasicUser != "deploy-user" || gotBasicPass != "dt-secret" {
		t.Errorf("BasicAuth = (%q, %q), want (deploy-user, dt-secret)", gotBasicUser, gotBasicPass)
	}
}

// basic 缺 username 必须报清晰错误(不得静默发空用户名请求)。
func TestDownloadBasicRequiresUsername(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("缺 username 的请求不得发出")
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "pkg.tar.gz")
	err := Download(context.Background(), srv.Client(), srv.URL,
		&Auth{Type: "basic", Token: "dt-secret"}, dest)
	if err == nil {
		t.Fatal("want username required error")
	}
	if got := err.Error(); got == "" || !containsAll(got, "basic", "username") {
		t.Errorf("error = %q, want 指明 basic 缺 username", got)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func TestDownloadFailsOnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "pkg.tar.gz")
	if err := Download(context.Background(), srv.Client(), srv.URL, nil, dest); err == nil {
		t.Fatal("expected error on 404")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("失败下载不得留下目标文件")
	}
}

func TestVerifySHA256(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	os.WriteFile(p, []byte("hello"), 0o644)
	sum := sha256.Sum256([]byte("hello"))
	want := hex.EncodeToString(sum[:])
	if err := VerifySHA256(p, want); err != nil {
		t.Errorf("expected match: %v", err)
	}
	if err := VerifySHA256(p, "deadbeef"); err == nil {
		t.Error("expected mismatch error")
	}
}

func TestExtractTarGzPreservesModeAndListsFiles(t *testing.T) {
	src := filepath.Join(t.TempDir(), "p.tar.gz")
	writeTarGz(t, src, map[string]struct {
		data string
		mode int64
	}{
		"run.sh":       {"#!/bin/sh\n", 0o755},
		"lib/libx.so":  {"ELF", 0o644},
		"manifest.yaml": {"manifest_version: 1\n", 0o644},
	})
	dest := t.TempDir()
	files, err := ExtractTarGz(src, dest)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(files) != 3 {
		t.Errorf("files = %v", files)
	}
	st, err := os.Stat(filepath.Join(dest, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Errorf("run.sh mode = %o, want 755", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dest, "lib/libx.so")); err != nil {
		t.Errorf("nested file missing: %v", err)
	}
}

func TestExtractTarGzRejectsTraversal(t *testing.T) {
	for name, entry := range map[string]string{
		"dotdot":   "../evil.sh",
		"absolute": "/etc/evil",
	} {
		t.Run(name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "p.tar.gz")
			writeTarGz(t, src, map[string]struct {
				data string
				mode int64
			}{entry: {"x", 0o644}})
			if _, err := ExtractTarGz(src, t.TempDir()); err == nil {
				t.Fatalf("expected traversal rejection for %q", entry)
			}
		})
	}
}

// 下载超过 maxDownloadSize 字节必须中止,不留残档,且错误可 errors.Is 分类。
func TestDownloadRejectsOversizedResponse(t *testing.T) {
	// 临时把上限压到 16 字节,避免测试生成 GiB 级数据
	orig := maxDownloadSize
	maxDownloadSize = 16
	defer func() { maxDownloadSize = orig }()

	payload := make([]byte, 64) // 64 > 16,必然超限
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "oversized.tar.gz")
	err := Download(context.Background(), srv.Client(), srv.URL, nil, dest)
	if err == nil {
		t.Fatal("expected oversize rejection, got nil")
	}
	if !errors.Is(err, ErrDownloadTooLarge) {
		t.Errorf("err = %v, want errors.Is(ErrDownloadTooLarge)", err)
	}
	// 失败路径不得留下目标文件
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("oversize download must not leave target file, stat: %v", statErr)
	}
}
