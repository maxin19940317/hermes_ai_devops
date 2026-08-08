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

// ProbeAndroidSOCChain 按 socProbeChain 探测,返回**所有**通过 ValidSOC 校验的
// 值(保持链上顺序,去重);全部取不到返回空切片。
//
// 为什么要返回整条链而不是首个命中(2026-08-08 A1):别名表(AGENT_SOC_ALIASES /
// 服务端 DEVICE_SOC_ALIASES)按惯例以**平台代号**为键(trinket→QCM6125),
// 而链的第一跳 ro.soc.model 给的是**型号串**。只拿首个值去查别名,会出现
// "别名本可以救、却没机会参与"的情况:直接匹配不上、别名也查不到,
// 于是预检报 soc mismatch、心跳注册的 soc 匹配不上任何 manifest(变体被静默
// 判 SKIPPED)。调用方必须对每个候选依次尝试"直接匹配 → 别名匹配"。
func ProbeAndroidSOCChain(ctx context.Context, runner Runner, serial string) []string {
	var out []string
	seen := map[string]bool{}
	for _, prop := range socProbeChain {
		soc, err := getPropQuiet(ctx, runner, serial, prop)
		if err != nil || soc == "" {
			continue
		}
		if !ValidSOC(soc) || seen[soc] {
			continue
		}
		seen[soc] = true
		out = append(out, soc)
	}
	return out
}

// ProbeAndroidSOC 返回链上第一个有效值(设备身份的展示/上报缺省值);
// 全部取不到返回 ""。**做匹配判断时不要用它**——用 ProbeAndroidSOCChain
// 遍历整条链,理由见其 doc comment。
func ProbeAndroidSOC(ctx context.Context, runner Runner, serial string) string {
	chain := ProbeAndroidSOCChain(ctx, runner, serial)
	if len(chain) == 0 {
		return ""
	}
	return chain[0]
}

// ResolveSOCAlias 在候选链上依次尝试别名表,返回第一个命中的**规范化型号**与 true。
// alias 值可能是脏配置(历史上配错过 "QCM6125;idp:QCS6490"),按 ; , 空格拆分,
// 取第一个非空片段——与服务端 SOC 清洗语义一致。
func ResolveSOCAlias(chain []string, aliases map[string]string) (string, bool) {
	for _, soc := range chain {
		alias, ok := aliases[soc]
		if !ok {
			// 别名表键可能按小写代号归一(服务端语义),再试一次
			alias, ok = aliases[strings.ToLower(soc)]
		}
		if !ok {
			continue
		}
		for _, cand := range strings.FieldsFunc(alias, func(r rune) bool {
			return r == ';' || r == ',' || r == ' '
		}) {
			if cand != "" {
				return cand, true
			}
		}
	}
	return "", false
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
