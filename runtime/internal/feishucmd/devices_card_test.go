package feishucmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"hermes-devops/runtime/internal/store"
)

type failingDeviceCardSender struct {
	err   error
	texts []string
}

func (s *failingDeviceCardSender) SendText(_ context.Context, text string) error {
	s.texts = append(s.texts, text)
	return nil
}

func (s *failingDeviceCardSender) SendCard(context.Context, any) error {
	return s.err
}

func TestDevicesFallsBackToTextWhenCardSendFails(t *testing.T) {
	st := store.NewMemStore()
	if err := st.UpsertClientDevices(ctx, store.Client{ClientID: "c1"}, []store.Device{{
		DeviceID:    "dev-1",
		Serial:      "dev-1",
		DisplayName: "SM6225-dev-1",
		ClientID:    "c1",
		SOC:         "SM6225",
	}}); err != nil {
		t.Fatal(err)
	}

	sender := &failingDeviceCardSender{err: errors.New("card API unavailable")}
	e := &Executor{Store: st, CardSender: sender}
	got, err := e.devices(ctx, nil)
	if err != nil {
		t.Fatalf("devices: %v", err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatal("卡片发送失败后 devices 返回空文本")
	}
	if !strings.Contains(got, "SM6225-dev-1") {
		t.Fatalf("降级文本未包含设备名: %q", got)
	}
}

// TestRenderDeviceTableCard:卡片 = schema 2.0 table(2026-08-14 升级,
// 支持表头高亮 + 相邻行底纹)。列 = 设备/系统/架构/内存;设备名必须完整含
// serial(ListFleet 漏选 serial 曾导致截断成 "SOC-",2026-08-07 修复)。
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
	// schema 2.0 + table 元素
	if !strings.Contains(js, `"schema":"2.0"`) {
		t.Errorf("缺 schema 2.0: %s", js[:200])
	}
	if !strings.Contains(js, `"tag":"table"`) {
		t.Errorf("缺 table 元素: %s", js[:300])
	}
	// 列名(display_name):设备/系统/架构/内存/磁盘
	for _, col := range []string{"设备", "系统", "架构", "内存", "磁盘"} {
		if !strings.Contains(js, col) {
			t.Errorf("卡片缺列 %q: %s", col, js[:300])
		}
	}
	// 设备名完整(含 serial)
	if !strings.Contains(js, "QCS6490-825485946") || !strings.Contains(js, "QCM6125-513cd3de") {
		t.Errorf("卡片缺完整设备名(含 serial): %s", js[:400])
	}
	// 斑马纹:2 行交替 blue/grey 背景色
	if !strings.Contains(js, `"background":"blue"`) || !strings.Contains(js, `"background":"grey"`) {
		t.Errorf("卡片缺斑马纹(blue/grey 交替): %s", js[:400])
	}
	// 设备名列加粗(表头高亮的补充)
	if !strings.Contains(js, "**QCS6490-825485946**") {
		t.Errorf("卡片缺设备名列加粗: %s", js[:400])
	}
	// 空列表 → 空卡片(调用方回纯文本)
	empty, err := renderDeviceTableCard(nil)
	if err != nil || empty != nil {
		t.Errorf("empty = %v, %v; want nil,nil", empty, err)
	}
}

// TestDeviceRowFromStatus:完整 FleetDevice → 表格行(显示名/系统/架构/内存/磁盘)。
func TestDeviceRowFromStatus(t *testing.T) {
	mb := int64(7304)
	diskT := int64(65536)
	diskF := int64(32768)
	d := store.FleetDevice{
		Device: store.Device{
			DeviceID: "825485946", Serial: "825485946", DisplayName: "QCS6490-825485946",
			OS: "linux", SOC: "QCS6490", ABI: "arm64-v8a", MemTotalMB: &mb,
			DiskTotalMB: &diskT, DiskFreeMB: &diskF,
		},
		Status: store.DeviceIdle,
	}
	row := deviceRowFromStatus(d)
	want := []string{"QCS6490-825485946", "Linux", "arm64-v8a", "7.1GB", "64.0GB/32.0GB"}
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

// TestDiskText:磁盘 "总/可用" 渲染;任一缺失 → 该侧 "-";全缺 → "-"。
func TestDiskText(t *testing.T) {
	big := ptr(65536)
	small := ptr(32768)
	cases := []struct {
		name  string
		total *int64
		free  *int64
		want  string
	}{
		{"both", big, small, "64.0GB/32.0GB"},
		{"only total", big, nil, "64.0GB/-"},
		{"only free", nil, small, "-/32.0GB"},
		{"neither", nil, nil, "-"},
	}
	for _, c := range cases {
		if got := diskText(c.total, c.free); got != c.want {
			t.Errorf("%s: diskText = %q, want %q", c.name, got, c.want)
		}
	}
}

func ptr(n int64) *int64 { return &n }

// okCardSender 卡片与文本都成功,用于观察"卡片成功后是否还多发了文本"。
type okCardSender struct {
	texts []string
	cards int
}

func (s *okCardSender) SendText(_ context.Context, text string) error {
	s.texts = append(s.texts, text)
	return nil
}

func (s *okCardSender) SendCard(context.Context, any) error {
	s.cards++
	return nil
}

// A5:卡片发送成功后,devices 返回 ""(约定"已回过了"),HandleMessage 不得
// 再发一条空文本消息——否则每次成功调用都在卡片后多出一个空气泡,
// 或被飞书拒收并刷错误日志。
func TestDevicesCardSuccessSendsNoTrailingEmptyText(t *testing.T) {
	st := store.NewMemStore()
	if err := st.UpsertClientDevices(ctx, store.Client{ClientID: "c1"}, []store.Device{{
		DeviceID:    "dev-1",
		Serial:      "dev-1",
		DisplayName: "SM6225-dev-1",
		ClientID:    "c1",
		SOC:         "SM6225",
	}}); err != nil {
		t.Fatal(err)
	}

	sender := &okCardSender{}
	e := &Executor{
		Store:      st,
		Sender:     sender,
		CardSender: sender,
		Whitelist:  map[string]bool{"ou_1": true},
	}
	e.HandleMessage(ctx, "ou_1", "devices")

	if sender.cards != 1 {
		t.Fatalf("SendCard 调用 %d 次, want 1(卡片路径未生效,本用例无从断言)", sender.cards)
	}
	if len(sender.texts) != 0 {
		t.Errorf("卡片成功后仍发了 %d 条文本: %q; want 0", len(sender.texts), sender.texts)
	}
}
