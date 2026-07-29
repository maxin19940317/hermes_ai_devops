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
	"encoding/xml"
	"io"
	"regexp"
	"strings"
)

// 提取规则常量(契约注释中的上限)。
const (
	contextLines           = 50       // 命中行上下文 ±50 行
	maxMatchesPerSignature = 3        // 每签名最多保留 3 处命中
	maxJunitFailures       = 20       // junit 失败最多 20 条
	maxJunitMessageBytes   = 2 << 10  // junit message 截断 2KB
	contextBudgetBytes     = 96 << 10 // 签名上下文总量预算,逼近 100KB 整体目标即截断
	// 兜底摘录(全部签名未命中时):单文件上限与 logcat 错误行上限,
	// 总量同样受 contextBudgetBytes 约束——有界,不是全量灌入。
	excerptFileBytes      = 16 << 10
	excerptLogcatMaxLines = 50
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

	SignaturesHitReported []string           // 设备自报(result.json),原样透传
	Metrics               map[string]float64 // 原始指标快照,原样透传

	Signatures []Signature
	Files      map[string]io.Reader // 键:"logcat"|"stdout"|"stderr"|"junit";缺键 = 该证据缺失
	Missing    []string             // 调用方已知的缺失文件名,透传进 inputs.missing
}

// ---- 输出结构(与 contracts/evidence.schema.json 一一对应)----

type Excerpt struct {
	File      string `json:"file"` // stdout.log / stderr.log / logcat.txt
	Kind      string `json:"kind"` // tail | error_lines
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
}

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
	// Excerpts 兜底摘录:全部签名未命中时给出有界原文(stdout/stderr 尾部 +
	// logcat 错误行);否则 Analyzer 只见文件元数据,只能回答"证据不足"(v2 新增)。
	Excerpts  []Excerpt `json:"excerpts,omitempty"`
	Truncated bool      `json:"truncated,omitempty"`
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
	LineNo    int    `json:"line_no"` // 从 1 起的原文件全局行号
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

// Extract 执行确定性证据提取。任何缺失/异常都降级进输出,不返回 error。
func Extract(in Input) Evidence {
	ev := Evidence{
		EvidenceVersion:       2,
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

	results := make([]SignatureResult, len(in.Signatures))
	compiled := make(map[int]*regexp.Regexp)
	matchers := make(map[string][]streamMatcher)
	for i, sig := range in.Signatures {
		results[i] = SignatureResult{
			ID: sig.ID, Where: sig.Where, Classify: sig.Classify,
			Matches: make([]Match, 0),
		}
		re, err := regexp.Compile(sig.Pattern)
		if err != nil {
			results[i].Error = "regex compile: " + err.Error()
			continue
		}
		compiled[i] = re
		matchers[sig.Where] = append(matchers[sig.Where], streamMatcher{index: i, re: re})
	}

	// 每个日志只做一次有界状态的完整流式扫描。
	scans := make(map[string]*streamScan)
	for _, key := range []string{"logcat", "stdout", "stderr"} {
		if r, ok := in.Files[key]; ok && r != nil {
			scans[key] = scanStream(r, matchers[key], key == "logcat")
			if scans[key].truncated || scans[key].readErr != nil {
				ev.Truncated = true
			}
		}
	}

	// ---- 签名结果组装(按声明序)----
	used := 0 // 已用上下文预算
	totalMatched := 0
	budgetOut := false
	for i, sig := range in.Signatures {
		res := results[i]
		if _, ok := compiled[i]; !ok {
			ev.Signatures = append(ev.Signatures, res)
			continue
		}
		scan := scans[sig.Where]
		if scan == nil {
			res.Error = "log missing: " + sig.Where
			ev.Signatures = append(ev.Signatures, res)
			continue
		}
		candidates := scan.candidates[i]
		if scan.readErr != nil && len(candidates) == 0 {
			res.Error = "read " + sig.Where + ": " + scan.readErr.Error()
			ev.Signatures = append(ev.Signatures, res)
			continue
		}
		if budgetOut {
			res.Error = "context budget exhausted"
			ev.Signatures = append(ev.Signatures, res)
			continue
		}
		for _, candidate := range candidates {
			ctx := candidate.context()
			if used+len(ctx) > contextBudgetBytes {
				budgetOut = true
				ev.Truncated = true
				break
			}
			used += len(ctx)
			res.Matches = append(res.Matches, Match{
				LineNo: candidate.lineNo, Context: ctx, Truncated: candidate.truncated,
			})
		}
		if budgetOut {
			// 预算在本次扫描中耗尽:无命中则该签名降级(Error 含 budget),
			// 已有部分命中则标记最后一条 truncated。
			if len(res.Matches) == 0 {
				res.Error = "context budget exhausted"
			} else {
				res.Matches[len(res.Matches)-1].Truncated = true
			}
		} else if scan.overflow[i] && len(res.Matches) > 0 {
			res.Matches[len(res.Matches)-1].Truncated = true
		}
		if scan.readErr != nil && len(res.Matches) > 0 {
			res.Matches[len(res.Matches)-1].Truncated = true
			ev.Truncated = true
		}
		res.Matched = len(res.Matches) > 0
		if res.Matched {
			totalMatched++
		}
		ev.Signatures = append(ev.Signatures, res)
	}

	// ---- junit 失败解析(非 XML 等解析失败降级为空,不报错)----
	if r, ok := in.Files["junit"]; ok && r != nil {
		ev.JunitFailures = parseJunit(r)
	}

	// ---- 兜底摘录(契约 v2):全部签名未命中时提供有界原文——
	// 否则 evidence 只有文件元数据,Analyzer 只能回答"证据不足"
	// (实证:2026-07-27 p56 SNPE_1.68 seg 模型错误全漏签名,Hermes 无法分析)。
	if totalMatched == 0 {
		ev.Excerpts = buildStreamExcerpts(scans)
	}

	// ---- inputs.attachments / truncated_files(固定顺序,确定性输出)----
	for _, key := range fileKeys {
		if key == "junit" {
			if _, ok := in.Files[key]; ok {
				// junit 只走流式解析,大小未知,记 0 占位。
				name := fileNames[key]
				ev.Inputs.Attachments = append(ev.Inputs.Attachments,
					Attachment{Name: name, ObjectKey: name})
			}
			continue
		}
		scan := scans[key]
		if scan == nil {
			continue
		}
		name := fileNames[key]
		ev.Inputs.Attachments = append(ev.Inputs.Attachments,
			Attachment{Name: name, ObjectKey: name, Size: scan.size})
		if scan.truncated {
			ev.Inputs.TruncatedFiles = append(ev.Inputs.TruncatedFiles, name)
		}
	}
	return ev
}

// logcatErrLine 匹配 logcat 的错误/致命行(级别列 E/F)。
var logcatErrLine = regexp.MustCompile(` [EF] `)

// buildStreamExcerpts 构造兜底摘录:stdout/stderr 尾部 + logcat 全文件中有界保留的最早错误行。
func buildStreamExcerpts(scans map[string]*streamScan) []Excerpt {
	out := make([]Excerpt, 0, 3)
	budget := contextBudgetBytes
	tail := func(key, name string) {
		if budget <= 0 {
			return
		}
		scan := scans[key]
		if scan == nil || scan.readErr != nil || len(scan.tail) == 0 {
			return
		}
		limit := excerptFileBytes
		if budget < limit {
			limit = budget
		}
		content, truncated := tailLines(scan.tail, limit)
		if content == "" {
			return
		}
		out = append(out, Excerpt{
			File: name, Kind: "tail", Content: content,
			Truncated: truncated || scan.lineCount > len(scan.tail),
		})
		budget -= len(content)
	}
	tail("stdout", "stdout.log")
	tail("stderr", "stderr.log")

	if budget > 0 {
		if scan := scans["logcat"]; scan != nil && scan.readErr == nil {
			if len(scan.errorLines) > 0 {
				limit := excerptFileBytes
				if budget < limit {
					limit = budget
				}
				content, truncated := headLines(scan.errorLines, limit)
				out = append(out, Excerpt{File: "logcat.txt", Kind: "error_lines",
					Content: content, Truncated: truncated || len(scan.errorLines) >= excerptLogcatMaxLines})
			}
		}
	}
	return out
}

// tailLines 取行切片尾部,不超过 budget 字节;截断时丢弃开头半行。
func tailLines(lines []string, budget int) (string, bool) {
	total := 0
	lo := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		n := len(lines[i]) + 1
		if total+n > budget {
			break
		}
		total += n
		lo = i
	}
	if lo == len(lines) {
		return "", false
	}
	return strings.Join(lines[lo:], "\n"), lo > 0
}

// headLines 取行切片头部,不超过 budget 字节。
func headLines(lines []string, budget int) (string, bool) {
	total := 0
	hi := 0
	for i, ln := range lines {
		n := len(ln) + 1
		if total+n > budget {
			break
		}
		total += n
		hi = i + 1
	}
	if hi == 0 {
		return "", false
	}
	return strings.Join(lines[:hi], "\n"), hi < len(lines)
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
