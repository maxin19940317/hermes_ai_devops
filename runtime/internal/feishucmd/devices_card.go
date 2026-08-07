package feishucmd

import (
"encoding/json"
"fmt"
"strings"

"hermes-devops/runtime/internal/store"
)

// ---- 设备列表卡片(plain_text 逐行,2026-08-07) ----
// 飞书 lark_md 对 "设备名-数字" 的连字符解析异常(serial 被吞,如
// "QCS6490-825485946" 显示成 "QCS6490-");column_set 被拍平,table 组件不可用。
// 最可靠:纯文本(plain_text)元素,每行一台设备,不做任何 markdown 解析。

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

// deviceCardElement 是卡片元素:div 文本块(每行一台设备)。
type deviceCardElement struct {
Tag  string         `json:"tag"` // 恒为 div
Text deviceCardText `json:"text"`
}

type deviceCardText struct {
Tag     string `json:"tag"` // plain_text(不做 markdown 解析,防连字符被吞)
Content string `json:"content"`
}

// deviceTableRow 是表格一行(按列顺序:设备/系统/架构/内存)。
type deviceTableRow []string

// renderDeviceTableCard 渲染设备列表为飞书卡片(plain_text 逐行)。
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
for _, r := range rows {
// 每行一台设备:设备名 + 系统 · 架构 · 内存
line := r[0]
if len(r) > 1 {
line += "  " + strings.Join(r[1:], " · ")
}
card.Elements = append(card.Elements, deviceCardElement{
Tag:  "div",
Text: deviceCardText{Tag: "plain_text", Content: line},
})
}
return card, nil
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
