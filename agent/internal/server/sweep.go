package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// purgeRunPayload 删除一次运行的 SDK 负载(out_dir 下的 package.tar.gz 与
// package/ 解包子树)。任务终态后负载即失去用途:结果上报与崩溃恢复只依赖
// run-summary.json 与 device/ 下的小文件(reporter.build),附件上传也只取
// device/ 与 out_dir 根的日志(见 uploadAttachments 的注释),负载本身可从
// Registry 按 dispatch 里的 url+sha256 重新下载。一次运行的负载在数十到
// 数百 MB,不删 agent-runs 会随测试量线性吃满磁盘(2026-08-10)。
// 删除失败只记日志,不阻断上报。
func (s *Server) purgeRunPayload(outDir string) {
	for _, name := range []string{"package.tar.gz", "package"} {
		if err := os.RemoveAll(filepath.Join(outDir, name)); err != nil {
			s.logf("purge run payload %s: %v", filepath.Join(outDir, name), err)
		}
	}
}

// SweepRuns 删除已过保留期的运行目录:已终态、结果已上报且结束时间早于
// cutoff 的任务,其 out_dir 整体删除(此时目录里只剩小文件——SDK 负载已在
// 任务结束时被 purgeRunPayload 清除)。
//
// 目录一律经 store 记录定位,不从目录名反解 task_id(截断+哈希后不可逆)。
// 两类目录不在此处理,需人工清理:DB 无记录的孤儿目录(如 agent.db 被重置),
// 与结果未上报的任务目录(RecoverPending 仍需要其中的 run-summary.json)。
func (s *Server) SweepRuns(ctx context.Context, cutoff time.Time) {
	tasks, err := s.cfg.Store.ListRecordedTerminalBefore(ctx, cutoff)
	if err != nil {
		s.logf("sweep runs: %v", err)
		return
	}
	for _, t := range tasks {
		// 防御:只删 RunsRoot 的直接子目录。脏数据(空串、越界路径)
		// 不得变成任意目录删除。
		if !isRunsRootChild(s.cfg.RunsRoot, t.OutDir) {
			s.logf("sweep runs: skip %s(outside runs root)", t.OutDir)
			continue
		}
		if err := os.RemoveAll(t.OutDir); err != nil {
			s.logf("sweep runs: remove %s: %v", t.OutDir, err)
		}
	}
}

// isRunsRootChild 判定 dir 是否 root 的直接子目录(相对/绝对路径均受理)。
func isRunsRootChild(root, dir string) bool {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absDir)
	if err != nil || rel == "." || rel == "" {
		return false
	}
	return filepath.Dir(rel) == "." &&
		!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}
