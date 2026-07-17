// smoke-check — 在打包机上无设备校验测试包:
// 解压 → Manifest Schema 校验 → 逐文件 sha256,与 agent-cli PREPARING 阶段同一代码路径。
// 用法: go run ./smoke/check <pkg.tar.gz>
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"hermes-devops/agent/internal/artifact"
	"hermes-devops/agent/internal/manifest"
)

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: smoke-check <pkg.tar.gz>")
		return 1
	}
	dir, err := os.MkdirTemp("", "smoke-check-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer os.RemoveAll(dir)

	if _, err := artifact.ExtractTarGz(os.Args[1], dir); err != nil {
		fmt.Fprintln(os.Stderr, "extract:", err)
		return 1
	}
	m, err := manifest.Load(filepath.Join(dir, "manifest.yaml"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "manifest:", err)
		return 1
	}
	for _, df := range m.Deploy.Files {
		if err := artifact.VerifySHA256(filepath.Join(dir, filepath.FromSlash(df.Src)), df.SHA256); err != nil {
			fmt.Fprintln(os.Stderr, "deploy file integrity:", err)
			return 1
		}
	}
	fmt.Printf("check OK: platform=%s entry=%s timeout=%ds files=%d workdir=%s\n",
		m.Artifact.Platform, m.Test.Entry, m.Test.TimeoutSec, len(m.Deploy.Files), m.Deploy.Workdir)
	return 0
}
