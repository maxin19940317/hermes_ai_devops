package feishucmd

import (
	"strings"
	"testing"

	"hermes-devops/runtime/internal/store"
)

// TestRenderDeviceTableCardIncludesIDColumn:卡片表格必须含 ID 列(用户要求最左加 ID)。
func TestRenderDeviceTableCardIncludesIDColumn(t *testing.T) {
	rows := []deviceTableRow{
		{"825485946", "QCS6490-825485946", "Linux", "arm64-v8a", "7.3GB"},
		{"513cd3de", "QCM6125-513cd3de", "Android", "arm64-v8a", "3.5GB"},
	}
	card, err := renderDeviceTableCard(rows)
	if err != nil {
		t.Fatal(err)
	}
	js, err := cardJSON(card)
	if err != nil {
		t.Fatal(err)
	}
	// 表头含 ID 列
	if !strings.Contains(js, "**ID**") {
		t.Errorf("卡片缺 ID 列表头: %s", js[:200])
	}
	// 数据含设备 ID
	if !strings.Contains(js, "825485946") || !strings.Contains(js, "513cd3de") {
		t.Errorf("卡片缺设备 ID 数据: %s", js[:300])
	}
	// 结构:column_set 行数 = 表头 + 2 数据
	if strings.Count(js, `"column_set"`) != 3 {
		t.Errorf("column_set 行数 = %d, want 3", strings.Count(js, `"column_set"`))
	}
	// 空列表 → 空卡片(调用方回纯文本)
	empty, err := renderDeviceTableCard(nil)
	if err != nil || empty != nil {
		t.Errorf("empty = %v, %v; want nil,nil", empty, err)
	}
}

// TestDeviceRowFromStatus:完整 FleetDevice → 表格行(含 ID/显示名/系统/架构/内存)。
func TestDeviceRowFromStatus(t *testing.T) {
	mb := int64(7304)
	d := store.FleetDevice{
		Device: store.Device{
			DeviceID: "825485946", Serial: "825485946", DisplayName: "QCS6490-825485946",
			OS: "linux", SOC: "QCS6490", ABI: "arm64-v8a", MemTotalMB: &mb,
		},
		Status: store.DeviceIdle,
	}
	row := deviceRowFromStatus(d)
	want := []string{"825485946", "QCS6490-825485946", "Linux", "arm64-v8a", "7.1GB"}
	if len(row) != len(want) {
		t.Fatalf("row = %v, want %d 列", row, len(want))
	}
	for i := range want {
		if row[i] != want[i] {
			t.Errorf("row[%d] = %q, want %q", i, row[i], want[i])
		}
	}
}

// TestMemText:MB → 人类可读(≥1024 → GB 保留 1 位小数;nil → "-")。
func TestMemText(t *testing.T) {
	cases := []struct {
		mb   *int64
		want string
	}{
		{ptr(7304), "7.1GB"}, // 7304/1024 = 7.13 → %.1f = 7.1
		{ptr(1972), "1.9GB"}, // 1972/1024 = 1.93 → 1.9
		{ptr(512), "512MB"},
		{nil, "-"},
	}
	for _, c := range cases {
		if got := memText(c.mb); got != c.want {
			t.Errorf("memText(%v) = %q, want %q", c.mb, got, c.want)
		}
	}
}

func ptr(n int64) *int64 { return &n }
