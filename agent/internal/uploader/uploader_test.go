package uploader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// putRecord 记录服务端收到的一次 PUT。
type putRecord struct {
	path          string
	method        string
	body          []byte
	contentLength int64
	header        http.Header
}

// newPutServer 启动记录型假 MinIO;failPaths 中的路径返回 500。
func newPutServer(t *testing.T, failPaths map[string]int) (*httptest.Server, func() []putRecord) {
	t.Helper()
	var records []putRecord
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		records = append(records, putRecord{
			path:          r.URL.Path,
			method:        r.Method,
			body:          body,
			contentLength: r.ContentLength,
			header:        r.Header.Clone(),
		})
		if code, ok := failPaths[r.URL.Path]; ok {
			w.WriteHeader(code)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []putRecord { return records }
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUploadSuccessAssertsPutSemantics(t *testing.T) {
	srv, records := newPutServer(t, nil)
	dir := t.TempDir()
	content := "junit xml payload"
	path := writeFile(t, dir, "junit.xml", content)

	u := &Uploader{Client: srv.Client()}
	atts := u.Upload(t.Context(), []PresignedUpload{
		{ObjectKey: "runs/t1/junit.xml", URL: srv.URL + "/bucket/runs/t1/junit.xml", ExpiresAt: time.Now().Add(time.Hour)},
	}, map[string]string{"runs/t1/junit.xml": path})

	if len(atts) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(atts))
	}
	recs := records()
	if len(recs) != 1 {
		t.Fatalf("want 1 PUT, got %d", len(recs))
	}
	r := recs[0]
	if r.method != http.MethodPut {
		t.Errorf("method = %s, want PUT", r.method)
	}
	if string(r.body) != content {
		t.Errorf("body = %q, want %q", r.body, content)
	}
	if r.contentLength != int64(len(content)) {
		t.Errorf("Content-Length = %d, want %d", r.contentLength, len(content))
	}
	if got := r.header.Get("Content-Type"); got != "" {
		t.Errorf("unexpected Content-Type header %q(预签名外不得加头)", got)
	}

	sum := sha256.Sum256([]byte(content))
	att := atts[0]
	if att.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %s, want %s", att.SHA256, hex.EncodeToString(sum[:]))
	}
	if att.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", att.Size, len(content))
	}
	if att.ObjectKey != "runs/t1/junit.xml" {
		t.Errorf("object_key = %s", att.ObjectKey)
	}
	if att.Name != "junit.xml" {
		t.Errorf("name = %s, want junit.xml", att.Name)
	}
}

func TestUploadSkipsMissingLocalFile(t *testing.T) {
	srv, records := newPutServer(t, nil)
	dir := t.TempDir()
	existing := writeFile(t, dir, "stdout.log", "out")

	var logs []string
	u := &Uploader{Client: srv.Client(), Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	atts := u.Upload(t.Context(), []PresignedUpload{
		{ObjectKey: "runs/t1/result.json", URL: srv.URL + "/missing", ExpiresAt: time.Now().Add(time.Hour)},
		{ObjectKey: "runs/t1/stdout.log", URL: srv.URL + "/ok", ExpiresAt: time.Now().Add(time.Hour)},
	}, map[string]string{
		"runs/t1/result.json": filepath.Join(dir, "result.json"), // 不存在的固定键
		"runs/t1/stdout.log":  existing,
	})

	if len(atts) != 1 || atts[0].ObjectKey != "runs/t1/stdout.log" {
		t.Fatalf("attachments = %+v, want only stdout.log", atts)
	}
	if len(records()) != 1 {
		t.Fatalf("want 1 PUT, got %d", len(records()))
	}
	// 跳过须大声记日志,且不得泄露预签名 URL
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "runs/t1/result.json") {
		t.Errorf("skip 未记日志: %q", joined)
	}
	if strings.Contains(joined, srv.URL) {
		t.Errorf("日志泄露预签名 URL: %q", joined)
	}
}

func TestUploadSkipsUnmappedKey(t *testing.T) {
	srv, records := newPutServer(t, nil)
	u := &Uploader{Client: srv.Client()}
	atts := u.Upload(t.Context(), []PresignedUpload{
		{ObjectKey: "runs/t1/logcat.txt", URL: srv.URL + "/x"},
	}, map[string]string{})
	if len(atts) != 0 || len(records()) != 0 {
		t.Fatalf("atts=%v puts=%d, want none", atts, len(records()))
	}
}

func TestUploadSkipsExpiredWithoutHTTPCall(t *testing.T) {
	srv, records := newPutServer(t, nil)
	dir := t.TempDir()
	path := writeFile(t, dir, "stderr.log", "err")

	u := &Uploader{Client: srv.Client()}
	atts := u.Upload(t.Context(), []PresignedUpload{
		{ObjectKey: "runs/t1/stderr.log", URL: srv.URL + "/expired", ExpiresAt: time.Now().Add(-time.Minute)},
	}, map[string]string{"runs/t1/stderr.log": path})

	if len(atts) != 0 {
		t.Fatalf("expired entry uploaded: %+v", atts)
	}
	if len(records()) != 0 {
		t.Fatalf("expired entry triggered HTTP call")
	}
}

func TestUploadFailureDoesNotAffectOthers(t *testing.T) {
	srv, records := newPutServer(t, map[string]int{"/fail400": http.StatusBadRequest, "/fail500": http.StatusInternalServerError})
	dir := t.TempDir()
	okPath := writeFile(t, dir, "ok.log", "ok")

	var logs []string
	u := &Uploader{Client: srv.Client(), Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	atts := u.Upload(t.Context(), []PresignedUpload{
		{ObjectKey: "k/400", URL: srv.URL + "/fail400"},
		{ObjectKey: "k/500", URL: srv.URL + "/fail500"},
		{ObjectKey: "k/ok", URL: srv.URL + "/ok"},
	}, map[string]string{
		"k/400": okPath,
		"k/500": okPath,
		"k/ok":  okPath,
	})

	if len(atts) != 1 || atts[0].ObjectKey != "k/ok" {
		t.Fatalf("attachments = %+v, want only k/ok", atts)
	}
	if len(records()) != 3 {
		t.Fatalf("want 3 PUT attempts, got %d", len(records()))
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "k/400") || !strings.Contains(joined, "k/500") {
		t.Errorf("失败项未记日志: %q", joined)
	}
	if strings.Contains(joined, srv.URL) {
		t.Errorf("日志泄露预签名 URL: %q", joined)
	}
}

func TestUploadNetworkErrorDoesNotAffectOthers(t *testing.T) {
	// 启动后立即关闭,得到必连不上的地址
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	srv, _ := newPutServer(t, nil)
	dir := t.TempDir()
	okPath := writeFile(t, dir, "ok.log", "ok")

	u := &Uploader{Client: srv.Client(), Timeout: 5 * time.Second}
	atts := u.Upload(t.Context(), []PresignedUpload{
		{ObjectKey: "k/dead", URL: deadURL + "/x"},
		{ObjectKey: "k/ok", URL: srv.URL + "/ok"},
	}, map[string]string{"k/dead": okPath, "k/ok": okPath})

	if len(atts) != 1 || atts[0].ObjectKey != "k/ok" {
		t.Fatalf("attachments = %+v, want only k/ok", atts)
	}
}

func TestUploadDeterministicOrder(t *testing.T) {
	srv, _ := newPutServer(t, nil)
	dir := t.TempDir()
	content := "x"
	path := writeFile(t, dir, "f", content)

	keys := []string{"runs/t1/a", "runs/t1/b", "runs/t1/c", "runs/t1/d"}
	var pre []PresignedUpload
	files := map[string]string{}
	for _, k := range keys {
		pre = append(pre, PresignedUpload{ObjectKey: k, URL: srv.URL + "/" + k})
		files[k] = path
	}
	u := &Uploader{Client: srv.Client()}
	for i := 0; i < 5; i++ {
		atts := u.Upload(t.Context(), pre, files)
		if len(atts) != len(keys) {
			t.Fatalf("round %d: got %d attachments", i, len(atts))
		}
		for j, k := range keys {
			if atts[j].ObjectKey != k {
				t.Fatalf("round %d: order[%d] = %s, want %s", i, j, atts[j].ObjectKey, k)
			}
		}
	}
}

// 0 字节文件也必须显式发送 Content-Length: 0(Go 默认对空 body 改用
// chunked,S3/MinIO 会回 411)。回归验收:p44-rerun4 stdout.log 0 字节上传失败。
func TestUploadEmptyFileSendsContentLengthZero(t *testing.T) {
	var gotLen string
	var gotTE []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLen = r.Header.Get("Content-Length")
		gotTE = r.TransferEncoding
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	empty := filepath.Join(t.TempDir(), "empty.log")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	u := &Uploader{Client: srv.Client()}
	atts := u.Upload(context.Background(),
		[]PresignedUpload{{ObjectKey: "runs/t/stdout.log", URL: srv.URL}},
		map[string]string{"runs/t/stdout.log": empty})
	if len(atts) != 1 {
		t.Fatalf("attachments = %d, want 1", len(atts))
	}
	if gotLen != "0" {
		t.Errorf("Content-Length = %q, want 0", gotLen)
	}
	if len(gotTE) != 0 {
		t.Errorf("TransferEncoding = %v, want none", gotTE)
	}
	if atts[0].Size != 0 {
		t.Errorf("size = %d, want 0", atts[0].Size)
	}
}

// 网络错误不得把含签名的预签名 URL 写进错误文本(设计 §6:URL 永不落日志)。
func TestUploadNetworkErrorDoesNotLeakURL(t *testing.T) {
	// 不可达地址:*url.Error 默认会内嵌完整 URL(含 X-Amz-Signature)
	signed := "http://127.0.0.1:1/bucket/k?X-Amz-Signature=deadbeef&X-Amz-Credential=secret"
	f := filepath.Join(t.TempDir(), "a.log")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	u := &Uploader{Client: &http.Client{Timeout: time.Second}}
	atts := u.Upload(context.Background(),
		[]PresignedUpload{{ObjectKey: "runs/t/a.log", URL: signed}},
		map[string]string{"runs/t/a.log": f})
	if len(atts) != 0 {
		t.Fatalf("预期上传失败, got %+v", atts)
	}
	// 失败路径只许出现 object_key,不许出现 URL 任何片段;
	// 直接调用 put 断言错误文本
	err := u.put(context.Background(), PresignedUpload{ObjectKey: "k", URL: signed}, f, 1)
	if err == nil {
		t.Fatal("预期错误")
	}
	if strings.Contains(err.Error(), "X-Amz-Signature") || strings.Contains(err.Error(), "deadbeef") {
		t.Errorf("错误文本泄露签名 URL: %v", err)
	}
}
