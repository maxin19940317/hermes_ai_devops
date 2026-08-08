package activity

import (
	"strconv"
	"strings"
)

// DevVersion 是未经 ldflags 注入版本时 Agent 上报的版本串
// (见 agent/internal/version)。
const DevVersion = "dev"

// compareVersion compares two semantic version strings.
// Returns -1 if a < b, 1 if a > b, 0 if equal.
// Handles "dev" as always < any formal version.
//
// 注意:本函数保持 "dev < 任何正式版本" 的排序语义,但**最低版本门禁不据此
// 拒绝 dev**——放行判断在 AcquireDevice 里显式短路(见 CLAUDE.md §12 Phase 3
// "dev 永远放行")。排序与门禁是两件事,不要把豁免塞进比较函数。
func compareVersion(a, b string) int {
	if a == b {
		return 0
	}
	if a == DevVersion {
		return -1
	}
	if b == DevVersion {
		return 1
	}
	ap := strings.Split(strings.TrimPrefix(a, "v"), ".")
	bp := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(ap) || i < len(bp); i++ {
		var an, bn int
		if i < len(ap) {
			an, _ = strconv.Atoi(ap[i])
		}
		if i < len(bp) {
			bn, _ = strconv.Atoi(bp[i])
		}
		if an < bn {
			return -1
		}
		if an > bn {
			return 1
		}
	}
	return 0
}
