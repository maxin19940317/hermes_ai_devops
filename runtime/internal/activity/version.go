package activity

import (
	"strconv"
	"strings"
)

// compareVersion compares two semantic version strings.
// Returns -1 if a < b, 1 if a > b, 0 if equal.
// Handles "dev" as always < any formal version.
func compareVersion(a, b string) int {
	if a == b {
		return 0
	}
	if a == "dev" {
		return -1
	}
	if b == "dev" {
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
