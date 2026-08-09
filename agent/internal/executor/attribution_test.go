package executor

import (
	"context"
	"errors"
	"testing"

	"hermes-devops/agent/internal/adb"
)

// TestClassifyFailure 覆盖 spec §5.1 判定表 + §5.3 两级存活复核 + §5.3.1
// resolve 阶段规则 + §6 防线 1 的全部要求行(见
// docs/superpowers/specs/2026-08-09-device-attribution-signal-design.md §11)。
func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		name       string
		stage      string
		err        error
		liveness   string // 一级 get-state 的 stdout;"" = 调用失败
		devicesOut string // 二级 adb devices -l 的 stdout
		devicesErr error  // 二级调用失败
		wantScope  string
	}{
		{"adb 二进制起不来", "precheck", &adb.LaunchError{}, "", "", nil, "client"},
		{"adb devices 非零退出", "resolve", errAdbServer, "", "", nil, "client"},
		{"resolve 无匹配(逻辑 serial ≠ transport)", "resolve", errNoMatch, "", "", nil, "none"},
		{"ctx 取消", "run", context.Canceled, "", "", nil, "none"},
		{"只读分区 mkdir 失败但设备活着", "deploy", errRemoteExit, "device", "", nil, "none"},
		{"设备掉线:一级非 device + 二级确认缺席", "run", errRemoteExit, "", "List of devices attached\n", nil, "device"},
		{"设备 offline:二级看到非 device 状态", "run", errRemoteExit, "", "List of devices attached\ndev1\toffline\n", nil, "device"},
		{"二级调用失败:排除不掉 server 故障", "run", errRemoteExit, "", "", errors.New("boom"), "none"},
		{"属性不符", "precheck", errSOCMismatch, "", "", nil, "none"},
		{"空间不足(不走复核)", "precheck", errNoSpace, "", "", nil, "device"},
		{"本地下载失败", "download", errors.New("http 500"), "", "", nil, "client"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeADB{getStateOut: tc.liveness, devicesList: tc.devicesOut, devicesErr: tc.devicesErr}
			e, _ := newExecutor(f)
			got := e.classifyFailure(context.Background(), "dev1", tc.stage, tc.err)
			if got != tc.wantScope {
				t.Fatalf("scope = %q, want %q", got, tc.wantScope)
			}
		})
	}
}
