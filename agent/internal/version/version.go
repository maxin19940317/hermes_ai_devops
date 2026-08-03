// Package version 是 Agent 构建时注入的版本号(LDFlags)。
// 构建时用 -ldflags "-X hermes-devops/agent/internal/version.Version=$(git describe --tags --always --dirty)"
// 注入;未注入时(go run / 开发构建)取缺省值 "dev"。
package version

// Version is the agent version string. It is set at build time via ldflags:
//
//	-ldflags "-X hermes-devops/agent/internal/version.Version=$(git describe --tags --always --dirty)"
//
// When not set (dev builds / go run), defaults to "dev".
var Version = "dev"
