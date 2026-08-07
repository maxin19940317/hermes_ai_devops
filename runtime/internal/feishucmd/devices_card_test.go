package feishucmd

import (
	"strings"
	"testing"

	"hermes-devops/runtime/internal/store"
)

// TestRenderDeviceTableCard:卡片 = markdown 多行,每行一台设备(加粗设备名 + 分隔)。
// column_set 在此飞书版本被拍平,table 组件不可用——改用 markdown 多行(2026-08-07)。
func TestRenderDeviceTableCard(t *testing.T) {
	rows := []deviceTableRow{
		{"QCS6490-825485946", "Linux", "arm64-v8a", "7.1GB"},
		{"QCM6125-513cd3de", "Android", "arm64-v8a", "3.5GB"},
	}
	card, err := renderDeviceTableCard(rows)
	if err != nil {
		t.Fatal(err)
	}
	js, err := cardJSON(card)
	if err != nil {
		t.Fatal(err)
	}
	// markdown 元素,每行含加粗设备名
	if !strings.Contains(js, `"tag":"markdown"`) {
		t.Errorf("卡片缺 markdown 元素: %s", js[:300])
	}
	if !strings.Contains(js, "**QCS6490-825485946**") || !strings.Contains(js, "**QCM6125-513cd3de**") {
		t.Errorf("卡片缺加粗设备名: %s", js[:400])
	}
	// 每行含 · 分隔的系统/架构/内存
	if !strings.Contains(js, "Linux · arm64-v8a · 7.1GB") {
		t.Errorf("卡片缺属性分隔: %s", js[:400])
	}
	// 不用 column_set
	if strings.Contains(js, "column_set") {
		t.Errorf("卡片不应含 column_set: %s", js[:400])
	}
	// 空列表 → 空卡片(调用方回纯文本)
	empty, err := renderDeviceTableCard(nil)
	if err != nil || empty != nil {
		t.Errorf("empty = %v, %v; want nil,nil", empty, err)
	}
}

// TestDeviceRowFromStatus:完整 FleetDevice → 表格行(显示名/系统/架构/内存)。
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
	want := []string{"QCS6490-825485946", "Linux", "arm64-v8a", "7.1GB"}
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
