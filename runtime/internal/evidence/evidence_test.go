package evidence

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const contractsDir = "../../../contracts"

func TestEmbeddedSchemaMatchesContract(t *testing.T) {
	want, err := os.ReadFile(filepath.Join(contractsDir, "evidence.schema.json"))
	if err != nil {
		t.Fatalf("read contracts schema: %v", err)
	}
	if !bytes.Equal(EmbeddedSchema, want) {
		t.Fatal("embedded evidence.schema.json 与 contracts/ 不一致,请重新拷贝(防契约漂移)")
	}
}

// numberedLines 生成 n 行 "prefix-0001" 风格的日志。
func numberedLines(prefix string, n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%s-%04d\n", prefix, i)
	}
	return b.String()
}

func baseInput() Input {
	return Input{
		TaskID: "task-1", Variant: "aarch64_Android_SNPE_2.21", Status: "FAILED",
		ExitCode: 1, DurationSec: 12.5,
		CasesTotal: 10, CasesPassed: 8, CasesFailed: 1, CasesSkipped: 1,
		SignaturesHitReported: []string{"native_crash"},
		Metrics:               map[string]float64{"fps": 30.5},
		Files:                 map[string]io.Reader{},
	}
}

func TestExtractSignatures(t *testing.T) {
	tests := []struct {
		name string
		in   func() Input
		want func(t *testing.T, ev Evidence)
	}{
		{
			name: "单命中",
			in: func() Input {
				in := baseInput()
				in.Signatures = []Signature{{ID: "native_crash", Where: "logcat", Pattern: "Fatal signal", Classify: "CODE"}}
				in.Files["logcat"] = strings.NewReader("boot ok\nFatal signal 11 (SIGSEGV)\nbye\n")
				return in
			},
			want: func(t *testing.T, ev Evidence) {
				s := ev.Signatures[0]
				if !s.Matched || len(s.Matches) != 1 {
					t.Fatalf("matches = %+v", s)
				}
				m := s.Matches[0]
				if m.LineNo != 2 || m.Truncated {
					t.Errorf("match = %+v", m)
				}
				if want := "boot ok\nFatal signal 11 (SIGSEGV)\nbye"; m.Context != want {
					t.Errorf("context = %q, want %q", m.Context, want)
				}
			},
		},
		{
			name: "多命中超过 3 处截断",
			in: func() Input {
				in := baseInput()
				in.Signatures = []Signature{{ID: "err", Where: "stdout", Pattern: "ERR", Classify: "UNKNOWN"}}
				in.Files["stdout"] = strings.NewReader(numberedLines("ERR", 10))
				return in
			},
			want: func(t *testing.T, ev Evidence) {
				s := ev.Signatures[0]
				if len(s.Matches) != 3 {
					t.Fatalf("matches = %d, want 3", len(s.Matches))
				}
				for i, m := range s.Matches {
					if m.LineNo != i+1 {
						t.Errorf("matches[%d].line_no = %d", i, m.LineNo)
					}
				}
				if !s.Matches[2].Truncated {
					t.Error("最后一条命中应标 truncated=true")
				}
				if s.Matches[0].Truncated || s.Matches[1].Truncated {
					t.Error("前两条命中不应标 truncated")
				}
			},
		},
		{
			name: "文件头边界上下文",
			in: func() Input {
				in := baseInput()
				in.Signatures = []Signature{{ID: "s", Where: "logcat", Pattern: "line-0001", Classify: "CODE"}}
				in.Files["logcat"] = strings.NewReader(numberedLines("line", 60))
				return in
			},
			want: func(t *testing.T, ev Evidence) {
				m := ev.Signatures[0].Matches[0]
				if m.LineNo != 1 {
					t.Fatalf("line_no = %d", m.LineNo)
				}
				lines := strings.Split(m.Context, "\n")
				// 命中在第 1 行:上文为空,下文 50 行 → 共 51 行
				if len(lines) != 51 || lines[0] != "line-0001" || lines[50] != "line-0051" {
					t.Errorf("context lines = %d, head=%q tail=%q", len(lines), lines[0], lines[len(lines)-1])
				}
			},
		},
		{
			name: "文件尾边界上下文",
			in: func() Input {
				in := baseInput()
				in.Signatures = []Signature{{ID: "s", Where: "logcat", Pattern: "line-0060", Classify: "CODE"}}
				in.Files["logcat"] = strings.NewReader(numberedLines("line", 60))
				return in
			},
			want: func(t *testing.T, ev Evidence) {
				m := ev.Signatures[0].Matches[0]
				if m.LineNo != 60 {
					t.Fatalf("line_no = %d", m.LineNo)
				}
				lines := strings.Split(m.Context, "\n")
				if len(lines) != 51 || lines[0] != "line-0010" || lines[50] != "line-0060" {
					t.Errorf("context lines = %d, head=%q tail=%q", len(lines), lines[0], lines[len(lines)-1])
				}
			},
		},
		{
			name: "非法正则降级",
			in: func() Input {
				in := baseInput()
				in.Signatures = []Signature{{ID: "bad", Where: "logcat", Pattern: "(", Classify: "CODE"}}
				in.Files["logcat"] = strings.NewReader("anything\n")
				return in
			},
			want: func(t *testing.T, ev Evidence) {
				s := ev.Signatures[0]
				if s.Matched || len(s.Matches) != 0 || !strings.Contains(s.Error, "regex compile") {
					t.Errorf("sig = %+v", s)
				}
			},
		},
		{
			name: "缺失文件与 missing 透传",
			in: func() Input {
				in := baseInput()
				in.Signatures = []Signature{
					{ID: "nomatch", Where: "logcat", Pattern: "zzz-never", Classify: "CODE"},
					{ID: "gone", Where: "stdout", Pattern: "ERR", Classify: "INFRA"},
				}
				in.Files["logcat"] = strings.NewReader("all good\n")
				in.Missing = []string{"junit.xml"}
				return in
			},
			want: func(t *testing.T, ev Evidence) {
				if ev.Signatures[0].Matched || ev.Signatures[0].Error != "" {
					t.Errorf("无匹配签名应为 matched=false 且无 error: %+v", ev.Signatures[0])
				}
				gone := ev.Signatures[1]
				if gone.Matched || !strings.Contains(gone.Error, "log missing") {
					t.Errorf("缺失日志签名: %+v", gone)
				}
				if len(ev.Inputs.Missing) != 1 || ev.Inputs.Missing[0] != "junit.xml" {
					t.Errorf("missing = %v", ev.Inputs.Missing)
				}
				if len(ev.SignaturesHitReported) != 1 || ev.SignaturesHitReported[0] != "native_crash" {
					t.Errorf("signatures_hit_reported = %v", ev.SignaturesHitReported)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := Extract(tt.in())
			tt.want(t, ev)
		})
	}
}

func TestExtractJunit(t *testing.T) {
	tests := []struct {
		name  string
		junit string
		want  func(t *testing.T, fs []JunitFailure)
	}{
		{
			name: "正常解析 failure 与 error",
			junit: `<?xml version="1.0"?>
<testsuite name="s">
  <testcase name="testA" classname="suite.A"><failure message="assert failed">stack</failure></testcase>
  <testcase name="testB" classname="suite.B"><error>segfault at 0x0</error></testcase>
  <testcase name="testOk" classname="suite.C"/>
</testsuite>`,
			want: func(t *testing.T, fs []JunitFailure) {
				if len(fs) != 2 {
					t.Fatalf("failures = %+v", fs)
				}
				if fs[0].Name != "testA" || fs[0].Classname != "suite.A" || fs[0].Message != "assert failed" {
					t.Errorf("fs[0] = %+v", fs[0])
				}
				if fs[1].Name != "testB" || fs[1].Message != "segfault at 0x0" {
					t.Errorf("fs[1] = %+v", fs[1])
				}
			},
		},
		{
			name:  "超过 20 条截断",
			junit: junitMany(25),
			want: func(t *testing.T, fs []JunitFailure) {
				if len(fs) != 20 {
					t.Errorf("failures = %d, want 20", len(fs))
				}
			},
		},
		{
			name: "message 截断 2KB",
			junit: `<testsuite><testcase name="big"><failure>` +
				strings.Repeat("x", 5000) + `</failure></testcase></testsuite>`,
			want: func(t *testing.T, fs []JunitFailure) {
				if len(fs) != 1 || len(fs[0].Message) != 2048 {
					t.Errorf("message len = %d", len(fs[0].Message))
				}
			},
		},
		{
			name:  "非 XML 降级为空",
			junit: "this is not xml at all <<<",
			want: func(t *testing.T, fs []JunitFailure) {
				if len(fs) != 0 {
					t.Errorf("failures = %+v, want empty", fs)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := baseInput()
			in.Files["junit"] = strings.NewReader(tt.junit)
			ev := Extract(in)
			tt.want(t, ev.JunitFailures)
		})
	}
}

// junitMany 生成含 n 个失败用例的 junit.xml。
func junitMany(n int) string {
	var b strings.Builder
	b.WriteString("<testsuite>")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `<testcase name="t%d"><failure message="m%d"/></testcase>`, i, i)
	}
	b.WriteString("</testsuite>")
	return b.String()
}

func TestExtractFileTruncation(t *testing.T) {
	// 构造 >8MB 的 logcat:头部标记 + 大量填充 + 尾部命中行
	var b strings.Builder
	b.WriteString("HEAD-MARKER\n")
	line := strings.Repeat("p", 199) + "\n" // 200B/行
	for b.Len() < maxFileBytes+1<<20 {
		b.WriteString(line)
	}
	b.WriteString("TAIL-MARKER fatal\n")

	in := baseInput()
	in.Signatures = []Signature{{ID: "s", Where: "logcat", Pattern: "TAIL-MARKER", Classify: "CODE"}}
	in.Files["logcat"] = strings.NewReader(b.String())
	ev := Extract(in)

	if len(ev.Inputs.TruncatedFiles) != 1 || ev.Inputs.TruncatedFiles[0] != "logcat.txt" {
		t.Fatalf("truncated_files = %v", ev.Inputs.TruncatedFiles)
	}
	s := ev.Signatures[0]
	if !s.Matched || len(s.Matches) != 1 {
		t.Fatalf("尾部命中应保留: %+v", s)
	}
	if strings.Contains(s.Matches[0].Context, "HEAD-MARKER") {
		t.Error("超限后上下文不应包含文件头")
	}
	for _, a := range ev.Inputs.Attachments {
		if a.Name == "logcat.txt" && a.Size > maxFileBytes {
			t.Errorf("attachment size = %d, 超过窗口上限", a.Size)
		}
	}
}

func TestExtractContextBudget(t *testing.T) {
	// 单个上下文 ≈ 60KB:101 行 × 600B;两个签名合计 > 96KB 预算
	big := func(mark string) string {
		var b strings.Builder
		for i := 0; i < 50; i++ {
			b.WriteString(strings.Repeat("x", 599) + "\n")
		}
		b.WriteString(mark + strings.Repeat("y", 599-len(mark)) + "\n")
		for i := 0; i < 50; i++ {
			b.WriteString(strings.Repeat("x", 599) + "\n")
		}
		return b.String()
	}
	in := baseInput()
	in.Signatures = []Signature{
		{ID: "a", Where: "stdout", Pattern: "MARK_A", Classify: "CODE"},
		{ID: "b", Where: "stderr", Pattern: "MARK_B", Classify: "INFRA"},
	}
	in.Files["stdout"] = strings.NewReader(big("MARK_A"))
	in.Files["stderr"] = strings.NewReader(big("MARK_B"))
	ev := Extract(in)

	if !ev.Truncated {
		t.Error("预算耗尽应置顶层 truncated=true")
	}
	if !ev.Signatures[0].Matched {
		t.Errorf("首个签名应在预算内命中: %+v", ev.Signatures[0])
	}
	if ev.Signatures[1].Matched || !strings.Contains(ev.Signatures[1].Error, "budget") {
		t.Errorf("次个签名应因预算耗尽降级: %+v", ev.Signatures[1])
	}
}

// TestEvidenceSchemaValidation 构造完整 Evidence,序列化后必须过契约校验。
func TestEvidenceSchemaValidation(t *testing.T) {
	full := Evidence{
		EvidenceVersion: 1, TaskID: "task-1", Variant: "v", Status: "FAILED",
		ExitCode: 1, DurationSec: 12.5,
		Cases:                 Cases{Total: 10, Passed: 8, Failed: 1, Skipped: 1},
		SignaturesHitReported: []string{"native_crash"},
		Signatures: []SignatureResult{{
			ID: "native_crash", Where: "logcat", Classify: "CODE", Matched: true,
			Matches: []Match{{LineNo: 3, Context: "a\nb\nc", Truncated: true}},
		}},
		JunitFailures: []JunitFailure{{Name: "testA", Classname: "suite.A", Message: "boom"}},
		Metrics:       map[string]float64{"fps": 30.5},
		Inputs: Inputs{
			Attachments:    []Attachment{{Name: "logcat.txt", ObjectKey: "logcat.txt", Size: 123}},
			Missing:        []string{"junit.xml"},
			TruncatedFiles: []string{"logcat.txt"},
		},
		Truncated: true,
	}
	validateJSON(t, full)

	// Extract 的实际产出(含空集合)同样必须过契约
	ev := Extract(baseInput())
	validateJSON(t, ev)
}

func validateJSON(t *testing.T, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := compiledSchema.Validate(doc); err != nil {
		t.Fatalf("schema 校验失败: %v\n%s", err, raw)
	}
}
