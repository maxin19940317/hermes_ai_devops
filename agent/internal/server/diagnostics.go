package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"hermes-devops/agent/internal/adb"
	"hermes-devops/agent/internal/reporter"
)

// DefaultDiagnosticsMaxBytes 是诊断探测输出的默认截断上限。
const DefaultDiagnosticsMaxBytes = 64 * 1024

// diagnosticsProbeTimeout 是单次探测的执行上限。
const diagnosticsProbeTimeout = 15 * time.Second

// defaultLogcatLines 是 logcat_tail 未指定 lines 时的默认行数。
const defaultLogcatLines = 200

// propNamePattern 是 getprop 属性名字符白名单(防注入,红线 §14)。
var propNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// DiagnosticsRequest 是契约 DiagnosticsRequest。
type DiagnosticsRequest struct {
	Probe string `json:"probe"`
	Args  *struct {
		Serial   string `json:"serial"`
		Lines    int    `json:"lines"`
		PropName string `json:"prop_name"`
	} `json:"args"`
}

// DiagnosticsResponse 是契约 DiagnosticsResponse。
type DiagnosticsResponse struct {
	Probe     string `json:"probe"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
}

// runDiagnostics 实现 POST /api/v1/diagnostics:仅白名单四探测,
// 未知探测/参数 400;输出截断到 DiagnosticsMaxBytes。禁止任意 shell(红线 §14)。
func (s *Server) runDiagnostics(w http.ResponseWriter, r *http.Request) {
	var req DiagnosticsRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields() // args 仅白名单键(serial/lines/prop_name)
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid diagnostics request: "+err.Error())
		return
	}

	args, err := s.buildProbeArgs(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), diagnosticsProbeTimeout)
	defer cancel()
	res, runErr := s.cfg.Runner.Run(ctx, args)
	out := res.Stdout
	if runErr != nil {
		out += fmt.Sprintf("\nerror: %v\n%s", runErr, res.Stderr)
	} else if res.ExitCode != 0 && res.Stderr != "" {
		out += res.Stderr
	}

	max := s.cfg.DiagnosticsMaxBytes
	if max <= 0 {
		max = DefaultDiagnosticsMaxBytes
	}
	truncated := false
	if len(out) > max {
		out = string(bytes.ToValidUTF8([]byte(out[:max]), nil))
		truncated = true
	}
	writeJSON(w, http.StatusOK, DiagnosticsResponse{
		Probe:     req.Probe,
		Output:    out,
		Truncated: truncated,
	})
}

// buildProbeArgs 把白名单探测映射到 adb 包的白名单命令构造器;
// 任何未知探测或参数组合都返回错误(调用方转 400)。
func (s *Server) buildProbeArgs(req DiagnosticsRequest) ([]string, error) {
	serial, lines, propName := "", 0, ""
	if req.Args != nil {
		serial, lines, propName = req.Args.Serial, req.Args.Lines, req.Args.PropName
	}

	switch req.Probe {
	case "adb_devices":
		if serial != "" || lines != 0 || propName != "" {
			return nil, fmt.Errorf("probe adb_devices takes no args")
		}
		return adb.Devices(), nil

	case "logcat_tail":
		if serial == "" {
			return nil, fmt.Errorf("probe logcat_tail requires args.serial")
		}
		if propName != "" {
			return nil, fmt.Errorf("probe logcat_tail does not take args.prop_name")
		}
		if lines < 0 || lines > 1000 {
			return nil, fmt.Errorf("args.lines must be within 1..1000")
		}
		if lines == 0 {
			lines = defaultLogcatLines
		}
		return adb.LogcatTail(serial, lines), nil

	case "df":
		if serial == "" {
			return nil, fmt.Errorf("probe df requires args.serial")
		}
		if lines != 0 || propName != "" {
			return nil, fmt.Errorf("probe df takes only args.serial")
		}
		workdir := s.cfg.DeviceWorkdir
		if workdir == "" {
			workdir = reporter.DefaultDeviceWorkdir
		}
		return adb.DiskFreeKB(serial, workdir), nil

	case "getprop":
		if serial == "" {
			return nil, fmt.Errorf("probe getprop requires args.serial")
		}
		if lines != 0 {
			return nil, fmt.Errorf("probe getprop does not take args.lines")
		}
		if !propNamePattern.MatchString(propName) {
			return nil, fmt.Errorf("args.prop_name must match %s", propNamePattern)
		}
		return adb.GetProp(serial, propName), nil
	}
	return nil, fmt.Errorf("unknown probe %q (whitelist: adb_devices, logcat_tail, df, getprop)", req.Probe)
}
