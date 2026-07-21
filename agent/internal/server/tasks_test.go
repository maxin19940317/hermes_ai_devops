package server

import "testing"

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
