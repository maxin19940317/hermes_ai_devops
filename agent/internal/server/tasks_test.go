package server

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"hermes-devops/agent/internal/reporter"
	"hermes-devops/agent/internal/uploader"
)

// task_id 由 Runtime 生成,含项目路径 '/' 与分隔符 ':'(合法);
// 净化后的 out_dir 目录名必须是单级、Windows 兼容的。
func TestSafeOutDirName(t *testing.T) {
	cases := map[string]string{
		"device-test-aios/algo_super_sdk-g0f3b2fe1-p43:aarch64_Android_SNPE_1.68:a1": "device-test-aios_algo_super_sdk-g0f3b2fe1-p43_aarch64_Android_SNPE_1.68_a1",
		"plain-id":        "plain-id",
		"with_underscore": "with_underscore",
		`back\slash`:      "back_slash",
		".":               "_",
		"..":              "_",
		"/":               "_",
	}
	for in, want := range cases {
		if got := safeOutDirName(in); got != want {
			t.Errorf("safeOutDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

// filesMapUploader 与 server_test.go 的 fakeUploader 不同:它同时记录
// presigned 键和 files map,用于断言 uploadFixedSet 的 filepath.Base 后缀
// 匹配不仅放行了正确的键,还把本地路径映射到了 wellKnownFiles 声明的位置。
type filesMapUploader struct {
	gotKeys  []string
	gotFiles map[string]string // objectKey → localPath
}

func (f *filesMapUploader) Upload(_ context.Context, p []uploader.PresignedUpload,
	files map[string]string) []reporter.Attachment {
	for _, x := range p {
		f.gotKeys = append(f.gotKeys, x.ObjectKey)
	}
	f.gotFiles = files
	return nil
}

// uploadFixedSet 后缀匹配(审查 #4 回归测试):ObjectKey 含多级前缀
// (如 runs/task_id/a2/result.json)时,filepath.Base 仍能匹配
// wellKnownFiles;若改用前缀匹配则会静默跳过所有项。
func TestUploadFixedSetSuffixMatching(t *testing.T) {
	outDir := t.TempDir()
	// 在 outDir 下按 wellKnownFiles 的相对路径创建文件,
	// 使 uploadFixedSet 不因文件缺失而跳过。
	for _, rel := range []string{
		"device/results/result.json",
		"device/results/junit.xml",
		"logcat.txt",
		"stdout.log",
		"stderr.log",
	} {
		p := filepath.Join(outDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	fake := &filesMapUploader{}
	srv := newTestServer(t, fake)

	d := Dispatch{TaskID: "suffix-test"}
	d.PresignedUploads = []struct {
		ObjectKey string `json:"object_key"`
		URL       string `json:"url"`
		ExpiresAt string `json:"expires_at"`
	}{
		// 标准前缀:runs/task_id/filename
		{ObjectKey: "runs/task_id/result.json", URL: "https://minio/r1"},
		{ObjectKey: "runs/task_id/logcat.txt", URL: "https://minio/lc"},
		// 多级前缀:runs/task_id/a2/filename(未来 Runtime 加 attempt 段)
		{ObjectKey: "runs/task_id/a2/junit.xml", URL: "https://minio/ju"},
		{ObjectKey: "some/deep/nested/stdout.log", URL: "https://minio/so"},
		// 不在 wellKnownFiles 中的键 → 不应出现在 files map 里
		{ObjectKey: "runs/task_id/unknown.dat", URL: "https://minio/unk"},
	}

	srv.uploadFixedSet(context.Background(), d, outDir)

	// 断言 files map:wellKnownFiles 中有的 4 个文件都应被映射;
	// unknown.dat 应被跳过(不在 files map 中)。
	wantInFiles := map[string]string{
		"runs/task_id/result.json":    "device/results/result.json",
		"runs/task_id/logcat.txt":     "logcat.txt",
		"runs/task_id/a2/junit.xml":   "device/results/junit.xml",
		"some/deep/nested/stdout.log": "stdout.log",
	}
	for objKey, wantRel := range wantInFiles {
		gotPath, ok := fake.gotFiles[objKey]
		if !ok {
			t.Errorf("objectKey %q missing from files map (filepath.Base 匹配失败?)", objKey)
			continue
		}
		wantPath := filepath.Join(outDir, filepath.FromSlash(wantRel))
		if gotPath != wantPath {
			t.Errorf("objectKey %q → localPath %q, want %q", objKey, gotPath, wantPath)
		}
	}
	// unknown.dat 不应被映射
	if _, ok := fake.gotFiles["runs/task_id/unknown.dat"]; ok {
		t.Error("unknown.dat should NOT be in files map (not in wellKnownFiles)")
	}

	// 断言 files map 恰好 4 项,不多不少
	if len(fake.gotFiles) != len(wantInFiles) {
		var gotKeys []string
		for k := range fake.gotFiles {
			gotKeys = append(gotKeys, k)
		}
		sort.Strings(gotKeys)
		t.Errorf("files map has %d entries, want %d; keys: %v",
			len(fake.gotFiles), len(wantInFiles), strings.Join(gotKeys, ", "))
	}
}
