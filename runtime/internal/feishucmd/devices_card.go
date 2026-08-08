package feishucmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"hermes-devops/runtime/internal/store"
)

// ---- 设备列表卡片(column_set 表格布局,2026-08-07) ----
// 用户实测确认 column_set 并排布局效果正确(此前竖排/截断的根因是
// ListFleet 漏选 serial/display_name,导致设备名 fallback 成 "SOC-")。

// deviceCard 是飞书 interactive 卡片的顶层结构。
type deviceCard struct {
	Config   deviceCardConfig    `json:"config"`
	Header   deviceCardHeader    `json:"header"`
	Elements []deviceCardElement `json:"elements"`
}

type deviceCardConfig struct {
	WideScreenMode bool `json:"wide_screen_mode"`
}

type deviceCardHeader struct {
	Title    deviceCardText `json:"title"`
	Template string         `json:"template"`
}

// deviceCardElement 是卡片元素:column_set(一行多列)。
type deviceCardElement struct {
	Tag      string         `json:"tag"`       // 恒为 column_set
	FlexMode string         `json:"flex_mode"` // none
	Columns  []deviceColumn `json:"columns"`
}

type deviceColumn struct {
	Tag           string          `json:"tag"`   // 恒为 column
	Width         string          `json:"width"` // weighted
	Weight        int             `json:"weight"`
	VerticalAlign string          `json:"vertical_align"` // top
	Elements      []deviceColElem `json:"elements"`
}

type deviceColElem struct {
	Tag  string         `json:"tag"` // div
	Text deviceCardText `json:"text"`
}

type deviceCardText struct {
	Tag     string `json:"tag"` // plain_text(不解析 markdown,防连字符被吞)
	Content string `json:"content"`
}

// deviceTableColumn 定义一列:标题 + 权重(控制宽度)。
type deviceTableColumn struct {
	Title  string
	Weight int
}

// deviceTableRow 是表格一行(按列顺序)。
type deviceTableRow []string

// deviceColumns 是设备表格的列定义(设备/系统/架构/内存)。
var deviceColumns = []deviceTableColumn{
	{"设备", 3},
	{"系统", 2},
	{"架构", 2},
	{"内存", 1},
}

// renderDeviceTableCard 渲染设备列表为飞书卡片(column_set 表格)。
// 返回卡片 JSON(直接作 SendCard 的 card 参数);空列表 → 返回空(调用方回纯文本)。
func renderDeviceTableCard(rows []deviceTableRow) (any, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	card := deviceCard{
		Config: deviceCardConfig{WideScreenMode: true},
		Header: deviceCardHeader{
			Title:    deviceCardText{Tag: "plain_text", Content: "📱 在线设备"},
			Template: "blue",
		},
		Elements: []deviceCardElement{},
	}
	// 表头行 + 数据行,全部用 column_set。
	all := make([]deviceTableRow, 0, len(rows)+1)
	header := make(deviceTableRow, 0, len(deviceColumns))
	for _, c := range deviceColumns {
		header = append(header, c.Title)
	}
	all = append(all, header)
	all = append(all, rows...)

	for _, r := range all {
		card.Elements = append(card.Elements, buildColumnSetRow(r))
	}
	return card, nil
}

// buildColumnSetRow 把一行单元格渲染为一个 column_set。
func buildColumnSetRow(row deviceTableRow) deviceCardElement {
	cols := make([]deviceColumn, 0, len(deviceColumns))
	for i, c := range deviceColumns {
		content := ""
		if i < len(row) {
			content = row[i]
		}
		cols = append(cols, deviceColumn{
			Tag: "column", Width: "weighted", Weight: c.Weight,
			VerticalAlign: "top",
			Elements: []deviceColElem{{
				Tag:  "div",
				Text: deviceCardText{Tag: "plain_text", Content: content},
			}},
		})
	}
	return deviceCardElement{Tag: "column_set", FlexMode: "none", Columns: cols}
}

// deviceRowFromStatus 把一台设备转成表格行(设备名/系统/架构/内存)。
func deviceRowFromStatus(d store.FleetDevice) deviceTableRow {
	return deviceTableRow{
		deviceDisplayNameFromFleet(d),
		osCN2(d.OS),
		d.ABI,
		memText(d.MemTotalMB),
	}
}

// deviceDisplayNameFromFleet 取设备显示名(Device.DisplayName 或 soc-serial 兜底)。
func deviceDisplayNameFromFleet(d store.FleetDevice) string {
	if d.DisplayName != "" {
		return d.DisplayName
	}
	if d.SOC != "" {
		return strings.ToUpper(d.SOC) + "-" + d.Serial
	}
	return "UNKNOWN-" + d.Serial
}

// osCN2 系统名中文化(Android/Linux 展示用)。
func osCN2(os string) string {
	switch strings.ToLower(os) {
	case "android":
		return "Android"
	case "linux":
		return "Linux"
	default:
		return os
	}
}

// memText 把 MB 换成人类可读文本(≥1024MB 显示 GB,保留 1 位小数)。
func memText(mb *int64) string {
	if mb == nil {
		return "-"
	}
	m := float64(*mb)
	if m >= 1024 {
		return fmt.Sprintf("%.1fGB", m/1024)
	}
	return fmt.Sprintf("%dMB", *mb)
}

// cardJSON 把卡片结构序列化(供测试断言)。
func cardJSON(card any) (string, error) {
	b, err := json.Marshal(card)
	return string(b), err
}
