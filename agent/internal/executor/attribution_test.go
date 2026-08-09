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
		// 评审 I-1:产物损坏(解压/manifest/逐文件 sha256 不符)是构建产物问题,
		// 不是这台 client 的问题;归 client 会让恰好领到坏包的健康 client
		// 被计成故障中(§1"记错账"同一种病),按 §5.1 末行保守归 none。
		{"包损坏(解压/manifest/逐文件 sha256 不符)", "unpack", errors.New("bad tar header"), "", "", nil, "none"},
		// 评审 I-2:顺序守卫——download 阶段被取消必须仍是 none,不能被
		// stage=="download" 分支抢先判成 client(ctx 取消/超时必须最先判)。
		{"下载被取消(顺序守卫)", "download", context.Canceled, "", "", nil, "none"},
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
