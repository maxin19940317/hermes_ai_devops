// Package evidence 实现 CLAUDE.md §12 Phase 2 的确定性证据提取器:
// 对设备测试产物(logcat/stdout/stderr/junit.xml)做签名正则匹配(含命中处
// ±50 行上下文)+ junit 失败解析 + 指标快照,产出几十 KB 级的 evidence.json,
// 作为 LLM Analyzer 的唯一输入——严禁把原始日志全量灌入 LLM。
//
// 本包是纯函数:无 I/O 依赖(文件内容以 io.Reader 传入)、无网络;
// 任何缺失/异常都降级记录到输出中,Extract 不返回 error。
// 输出结构对齐 contracts/evidence.schema.json(包内嵌副本,防漂移由单测保证)。
package evidence

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"io"
	"regexp"
	"strings"
)

// 提取规则常量(契约注释中的上限)。
const (
	contextLines          = 50          // 命中行上下文 ±50 行
	maxMatchesPerSignature = 3           // 每签名最多保留 3 处命中
	maxFileBytes          = 8 << 20     // 单文件读取上限 8MB,超出只留尾部
	maxJunitFailures      = 20          // junit 失败最多 20 条
	maxJunitMessageBytes  = 2 << 10     // junit message 截断 2KB
	contextBudgetBytes    = 96 << 10    // 签名上下文总量预算,逼近 100KB 整体目标即截断
)

// Signature 是一条失败签名声明(来自 variants.yaml 合并结果)。
type Signature struct {
	ID       string // 签名 id
	Where    string // 扫描目标:logcat|stdout|stderr
	Pattern  string // 正则
	Classify string // 分类:INFRA|BUILD|CODE|MODEL|DELEGATE|DEVICE|PERF|UNKNOWN
}

// Input 是 Extract 的全部输入(调用方已备好,无 I/O)。
type Input struct {
	TaskID, Variant, Status string
	ExitCode                int
	DurationSec             float64

	CasesTotal, CasesPassed, CasesFailed, CasesSkipped int

	SignaturesHitReported []string          // 设备自报(result.json),原样透传
	Metrics               map[string]float64 // 原始指标快照,原样透传

	Signatures []Signature
	Files      map[string]io.Reader // 键:"logcat"|"stdout"|"stderr"|"junit";缺键 = 该证据缺失
	Missing    []string             // 调用方已知的缺失文件名,透传进 inputs.missing
}

// ---- 输出结构(与 contracts/evidence.schema.json 一一对应)----

type Evidence struct {
	EvidenceVersion       int                `json:"evidence_version"`
	TaskID                string             `json:"task_id"`
	Variant               string             `json:"variant"`
	Status                string             `json:"status"`
	ExitCode              int                `json:"exit_code"`
	DurationSec           float64            `json:"duration_sec"`
	Cases                 Cases              `json:"cases"`
	SignaturesHitReported []string           `json:"signatures_hit_reported"`
	Signatures            []SignatureResult  `json:"signatures"`
	JunitFailures         []JunitFailure     `json:"junit_failures"`
	Metrics               map[string]float64 `json:"metrics,omitempty"`
	Inputs                Inputs             `json:"inputs"`
	Truncated             bool               `json:"truncated,omitempty"`
}

type Cases struct {
	Total   int `json:"total"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

type SignatureResult struct {
	ID       string  `json:"id"`
	Where    string  `json:"where"`
	Classify string  `json:"classify"`
	Matched  bool    `json:"matched"`
	Matches  []Match `json:"matches"`
	// Error 降级记录:正则编译失败、对应日志缺失、上下文预算耗尽等;
	// 不为空时 matched 语义不可用。
	Error string `json:"error,omitempty"`
}

type Match struct {
	LineNo    int    `json:"line_no"` // 从 1 起;文件超限截断时基于所保留的尾部窗口
	Context   string `json:"context"` // 命中行 ±50 行,文件头尾自然截短
	Truncated bool   `json:"truncated,omitempty"`
}

type JunitFailure struct {
	Name      string `json:"name"`
	Classname string `json:"classname,omitempty"`
	Message   string `json:"message,omitempty"`
}

type Inputs struct {
	Attachments    []Attachment `json:"attachments"`
	Missing        []string     `json:"missing"`
	TruncatedFiles []string     `json:"truncated_files,omitempty"`
}

type Attachment struct {
	Name string `json:"name"`
	// ObjectKey 由上传方(MinIO 路径)决定,提取器不可得,以证据文件名占位;
	// Analyzer 不依赖该字段取数。
	ObjectKey string `json:"object_key"`
	Size      int64  `json:"size,omitempty"`
}

// fileKeys 固定顺序,保证 attachments / truncated_files 输出确定。
var fileKeys = []string{"logcat", "stdout", "stderr", "junit"}

// fileNames 把 Files 键映射为证据文件名。
var fileNames = map[string]string{
	"logcat": "logcat.txt",
	"stdout": "stdout.log",
	"stderr": "stderr.log",
	"junit":  "junit.xml",
}

// fileWindow 是单文件读取后的行窗口(超限只留尾部 maxFileBytes)。
type fileWindow struct {
	lines     []string
	truncated bool
	size      int64
}

// readWindow 流式读完全文,只保留尾部 maxFileBytes 窗口(超限丢弃头部,
// 避免大文件占内存;丢弃窗口开头被截断的半行,窗口首个完整行视为第 1 行)。
func readWindow(r io.Reader) (*fileWindow, error) {
	buf := make([]byte, 0, maxFileBytes)
	tmp := make([]byte, 64<<10)
	truncated := false
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if len(buf) > maxFileBytes {
				truncated = true
				tail := make([]byte, maxFileBytes)
				copy(tail, buf[len(buf)-maxFileBytes:])
				buf = tail
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
	}
	if truncated {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	fw := &fileWindow{truncated: truncated, size: int64(len(buf))}
	sc := bufio.NewScanner(bytes.NewReader(buf))
	sc.Buffer(make([]byte, 64<<10), 1<<20) // 容忍单行最长 1MB
	for sc.Scan() {
		fw.lines = append(fw.lines, sc.Text())
	}
	return fw, nil
}

// Extract 执行确定性证据提取。任何缺失/异常都降级进输出,不返回 error。
func Extract(in Input) Evidence {
	ev := Evidence{
		EvidenceVersion:       1,
		TaskID:                in.TaskID,
		Variant:               in.Variant,
		Status:                in.Status,
		ExitCode:              in.ExitCode,
		DurationSec:           in.DurationSec,
		Cases:                 Cases{in.CasesTotal, in.CasesPassed, in.CasesFailed, in.CasesSkipped},
		SignaturesHitReported: append([]string{}, in.SignaturesHitReported...),
		Signatures:            make([]SignatureResult, 0, len(in.Signatures)),
		JunitFailures:         make([]JunitFailure, 0),
		Metrics:               in.Metrics,
		Inputs: Inputs{
			Attachments: make([]Attachment, 0, len(in.Files)),
			Missing:     append([]string{}, in.Missing...),
		},
	}

	// 重复文件(如 logcat 被多签名引用)只读一次。
	windows := map[string]*fileWindow{}
	readErr := map[string]error{}
	load := func(key string) (*fileWindow, error) {
		if w, ok := windows[key]; ok {
			return w, readErr[key]
		}
		r, ok := in.Files[key]
		if !ok || r == nil {
			windows[key] = nil
			return nil, nil
		}
		w, err := readWindow(r)
		windows[key] = w
		readErr[key] = err
		return w, err
	}

	// ---- 签名匹配(按声明序)----
	used := 0 // 已用上下文预算
	budgetOut := false
	for _, sig := range in.Signatures {
		res := SignatureResult{
			ID: sig.ID, Where: sig.Where, Classify: sig.Classify,
			Matches: make([]Match, 0),
		}
		re, err := regexp.Compile(sig.Pattern)
		if err != nil {
			res.Error = "regex compile: " + err.Error()
			ev.Signatures = append(ev.Signatures, res)
			continue
		}
		w, rerr := load(sig.Where)
		switch {
		case rerr != nil:
			res.Error = "read " + sig.Where + ": " + rerr.Error()
			ev.Signatures = append(ev.Signatures, res)
			continue
		case w == nil:
			res.Error = "log missing: " + sig.Where
			ev.Signatures = append(ev.Signatures, res)
			continue
		}
		if budgetOut {
			res.Error = "context budget exhausted"
			ev.Signatures = append(ev.Signatures, res)
			continue
		}
		overflow := false // 命中超过 maxMatchesPerSignature
		for i, line := range w.lines {
			if !re.MatchString(line) {
				continue
			}
			if len(res.Matches) >= maxMatchesPerSignature {
				overflow = true
				continue
			}
			lo := i - contextLines
			if lo < 0 {
				lo = 0
			}
			hi := i + contextLines + 1
			if hi > len(w.lines) {
				hi = len(w.lines)
			}
			ctx := strings.Join(w.lines[lo:hi], "\n")
			if used+len(ctx) > contextBudgetBytes {
				// 整体目标 <100KB:停止追加新命中
				budgetOut = true
				ev.Truncated = true
				break
			}
			used += len(ctx)
			res.Matches = append(res.Matches, Match{LineNo: i + 1, Context: ctx})
		}
		if budgetOut {
			// 预算在本次扫描中耗尽:无命中则该签名降级(Error 含 budget),
			// 已有部分命中则标记最后一条 truncated。
			if len(res.Matches) == 0 {
				res.Error = "context budget exhausted"
			} else {
				res.Matches[len(res.Matches)-1].Truncated = true
			}
		} else if overflow && len(res.Matches) > 0 {
			res.Matches[len(res.Matches)-1].Truncated = true
		}
		res.Matched = len(res.Matches) > 0
		ev.Signatures = append(ev.Signatures, res)
	}

	// ---- junit 失败解析(非 XML 等解析失败降级为空,不报错)----
	if r, ok := in.Files["junit"]; ok && r != nil {
		ev.JunitFailures = parseJunit(r)
	}

	// ---- inputs.attachments / truncated_files(固定顺序,确定性输出)----
	for _, key := range fileKeys {
		w := windows[key]
		if w == nil {
			if key == "junit" {
				if _, ok := in.Files["junit"]; ok {
					// junit 只走流式解析,未入行窗口缓存;大小未知,记 0 占位。
					name := fileNames[key]
					ev.Inputs.Attachments = append(ev.Inputs.Attachments,
						Attachment{Name: name, ObjectKey: name})
				}
			}
			continue
		}
		name := fileNames[key]
		ev.Inputs.Attachments = append(ev.Inputs.Attachments,
			Attachment{Name: name, ObjectKey: name, Size: w.size})
		if w.truncated {
			ev.Inputs.TruncatedFiles = append(ev.Inputs.TruncatedFiles, name)
		}
	}
	return ev
}

// parseJunit 流式解析 junit.xml:收集 testcase 下的 failure 与 error,
// name/classname 取自 testcase 属性,message 取元素 message 属性或文本。
// 最多 maxJunitFailures 条,message 截断 maxJunitMessageBytes;
// 解析失败(文件可能根本不是 XML)返回已收集部分,不报错。
func parseJunit(r io.Reader) []JunitFailure {
	out := make([]JunitFailure, 0)
	dec := xml.NewDecoder(r)
	var cur *JunitFailure // 当前 testcase
	inFail := false
	var msg strings.Builder
	flush := func() {
		if cur == nil || cur.Name == "" {
			return
		}
		m := cur.Message
		if m == "" {
			m = strings.TrimSpace(msg.String())
		}
		if len(m) > maxJunitMessageBytes {
			m = m[:maxJunitMessageBytes]
		}
		out = append(out, JunitFailure{Name: cur.Name, Classname: cur.Classname, Message: m})
	}
	for len(out) < maxJunitFailures {
		tok, err := dec.Token()
		if err != nil { // io.EOF 或语法错误:降级返回已收集部分
			return out
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "testcase":
				cur = &JunitFailure{}
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "name":
						cur.Name = a.Value
					case "classname":
						cur.Classname = a.Value
					}
				}
			case "failure", "error":
				if cur != nil {
					inFail = true
					msg.Reset()
					for _, a := range t.Attr {
						if a.Name.Local == "message" {
							cur.Message = a.Value
						}
					}
				}
			}
		case xml.CharData:
			// 限制收集量,避免巨型堆栈撑爆内存(最终只留 2KB)
			if inFail && msg.Len() < maxJunitMessageBytes*2 {
				msg.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "failure", "error":
				if inFail {
					inFail = false
					flush()
					cur.Message = "" // 同 testcase 可能有多个 failure/error
				}
			case "testcase":
				cur = nil
			}
		}
	}
	return out
}
