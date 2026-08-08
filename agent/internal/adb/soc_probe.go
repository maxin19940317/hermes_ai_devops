package adb

import (
	"context"
	"fmt"
	"strings"
)

// socProbeChain 是 Android SoC 自动探测链(按真实度优先,2026-08-07):
//
//  1. ro.soc.model    真实 SoC 型号(如 SM6225、SM8250、SDM845)
//  2. ro.chipname     部分高通设备提供(如 sdm845、sm8350)
//  3. ro.board.platform  平台代号(如 bengal、trinket、idp)
//  4. ro.product.board   板名(兜底)
//
// 心跳上报(reporter.ProbeDevices)与任务预检(executor.precheckAndroid)
// **必须共用同一链**——否则心跳按 ro.soc.model 调度成功、预检却只读
// ro.board.platform 而 soc mismatch(2026-08-08 Review P1)。
var socProbeChain = []string{
	"ro.soc.model",
	"ro.chipname",
	"ro.board.platform",
	"ro.product.board",
}

// ProbeAndroidSOC 按 socProbeChain 探测设备 SoC,返回第一个通过 validSOC
// 校验的值;全部取不到返回 ""。与 reporter.probeAndroidSOC 同一语义——
// 这是两套设备身份判断(心跳 vs 预检)的唯一共享实现。
func ProbeAndroidSOC(ctx context.Context, runner Runner, serial string) string {
	for _, prop := range socProbeChain {
		soc, err := getPropQuiet(ctx, runner, serial, prop)
		if err != nil || soc == "" {
			continue
		}
		if !ValidSOC(soc) {
			continue
		}
		return soc
	}
	return ""
}

// ValidSOC 校验 getprop 返回的 SoC 内容形态,拒绝 shell 错误文本被当做
// SoC 型号(老 adbd 合并 stderr 到 stdout 且不回传远程退出码)。
// SoC 型号可含大写(真实型号如 SM6225、SM8250)或小写(平台代号如 bengal、
// trinket),仅含字母数字与点、下划线、连字符。
func ValidSOC(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			(i > 0 && (r == '.' || r == '_' || r == '-'))
		if !ok {
			return false
		}
	}
	return true
}

// getPropQuiet 读单个 getprop,失败/非零退出静默(探测是尽力而为)。
func getPropQuiet(ctx context.Context, runner Runner, serial, prop string) (string, error) {
	res, err := runner.Run(ctx, GetProp(serial, prop))
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("getprop %s: exit=%d", prop, res.ExitCode)
	}
	return strings.TrimSpace(res.Stdout), nil
}
