package feishucmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"hermes-devops/runtime/internal/store"
)

// ---- 设备列表卡片(schema 2.0 table,2026-08-14) ----

// deviceCardHeader 是卡片头部(标题 + 颜色模板)。
type deviceCardHeader struct {
	Title    deviceCardText `json:"title"`
	Template string         `json:"template"`
}

type deviceCardText struct {
	Tag     string `json:"tag"` // plain_text
	Content string `json:"content"`
}

// deviceTableColumn 定义一列:标题 + 权重(列宽分配)。
type deviceTableColumn struct {
	Title  string
	Weight int
}

// deviceTableRow 是表格一行(按列顺序)。
type deviceTableRow []string

// deviceColumns 是设备表格的列定义(设备/系统/架构/内存/磁盘)。
var deviceColumns = []deviceTableColumn{
	{"设备", 3},
	{"系统", 2},
	{"架构", 2},
	{"内存", 1},
	{"磁盘(总/可用)", 2},
}

// ---- 设备列表卡片(schema 2.0 table,2026-08-14) ----
// 从 schema 1.0 column_set 升级为 schema 2.0 的 table 组件,以获得表格语义:
// 列名(display_name)+ 列宽 + 表头高亮 + 相邻行底纹(斑马纹,交替 cell 背景色)。
// 实测(2026-08-14 飞书 API):table 的 columns 支持 name/display_name/width;
// rows 是"以 column name 为键"的对象数组;cell 的 text 支持 markdown +
// text_style.background(背景色)。用户实测确认视觉正确。

// deviceTableCard 是 schema 2.0 卡片顶层结构。
type deviceTableCard struct {
	Schema string             `json:"schema"`
	Header deviceCardHeader   `json:"header"`
	Body   deviceCardBody     `json:"body"`
}

type deviceCardBody struct {
	Elements []deviceTableEl `json:"elements"`
}

type deviceTableEl struct {
	Tag     string             `json:"tag"` // 恒为 table
	Columns []deviceTableCol   `json:"columns"`
	Rows    []deviceTableRowObj `json:"rows"`
}

type deviceTableCol struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Width       string `json:"width,omitempty"`
}

// deviceTableRowObj 是一行:以 column name 为键,值为 cell。
type deviceTableRowObj map[string]deviceTableCell

type deviceTableCell struct {
	Text deviceCellText `json:"text"`
}

type deviceCellText struct {
	Tag       string          `json:"tag"` // markdown
	Content   string          `json:"content"`
	TextStyle *deviceCellStyle `json:"text_style,omitempty"`
}

type deviceCellStyle struct {
	Background string `json:"background,omitempty"` // blue | grey 等
}

// zebra 交替行底纹:偶数行 blue,奇数行 grey(2026-08-14,飞书实测支持)。
func zebraBG(rowIdx int) string {
	if rowIdx%2 == 0 {
		return "blue"
	}
	return "grey"
}

// renderDeviceTableCard 渲染设备列表为飞书 schema 2.0 table 卡片。
// 返回卡片(直接作 SendCard 的 card 参数);空列表 → 返回空(调用方回纯文本)。
func renderDeviceTableCard(rows []deviceTableRow) (any, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	// 表头列:name 用 ASCII 键,display_name 中文展示。
	cols := make([]deviceTableCol, 0, len(deviceColumns))
	for i, c := range deviceColumns {
		cols = append(cols, deviceTableCol{
			Name:        fmt.Sprintf("c%d", i),
			DisplayName: c.Title,
			Width:       tableColWidth(i),
		})
	}
	// 数据行 + 斑马纹;设备名列加粗(表头高亮的补充)。
	tableRows := make([]deviceTableRowObj, 0, len(rows))
	for i, r := range rows {
		bg := zebraBG(i)
		obj := deviceTableRowObj{}
		for j := range deviceColumns {
			content := ""
			if j < len(r) {
				content = r[j]
			}
			// 设备列(第 0 列)加粗,体现设备名是主信息。
			md := escapeCellText(content)
			if j == 0 {
				md = "**" + md + "**"
			}
			obj[fmt.Sprintf("c%d", j)] = deviceTableCell{
				Text: deviceCellText{
					Tag:       "markdown",
					Content:   md,
					TextStyle: &deviceCellStyle{Background: bg},
				},
			}
		}
		tableRows = append(tableRows, obj)
	}
	card := deviceTableCard{
		Schema: "2.0",
		Header: deviceCardHeader{
			Title:    deviceCardText{Tag: "plain_text", Content: "📱 在线设备"},
			Template: "blue",
		},
		Body: deviceCardBody{Elements: []deviceTableEl{{
			Tag:     "table",
			Columns: cols,
			Rows:    tableRows,
		}}},
	}
	return card, nil
}

// escapeCellText 转义 markdown 元字符(单元格内容动态,防注入/格式破坏)。
func escapeCellText(s string) string {
	r := strings.NewReplacer(
		"*", "\\*", "`", "\\`", "[", "\\[", "]", "\\]",
		"(", "\\(", ")", "\\)", "_", "\\_", "~", "\\~",
	)
	return r.Replace(s)
}

// tableColWidth 返回列的显示宽度(px;按内容量分配)。
func tableColWidth(i int) string {
	switch i {
	case 0:
		return "260px"
	case 4: // 磁盘
		return "180px"
	default:
		return "110px"
	}
}

// deviceRowFromStatus 把一台设备转成表格行(设备名/系统/架构/内存/磁盘)。
func deviceRowFromStatus(d store.FleetDevice) deviceTableRow {
	return deviceTableRow{
		deviceDisplayNameFromFleet(d),
		osCN2(d.OS),
		d.ABI,
		memText(d.MemTotalMB),
		diskText(d.DiskTotalMB, d.DiskFreeMB),
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

// diskText 渲染磁盘 "总/可用" 文本(如 "64GB/32GB");任一缺失 → "-"。
// 与 memText 同源风格(2026-08-11 加)。
func diskText(totalMB, freeMB *int64) string {
	t := memText(totalMB)
	f := memText(freeMB)
	if t == "-" && f == "-" {
		return "-"
	}
	return t + "/" + f
}

// cardJSON 把卡片结构序列化(供测试断言)。
func cardJSON(card any) (string, error) {
	b, err := json.Marshal(card)
	return string(b), err
}
