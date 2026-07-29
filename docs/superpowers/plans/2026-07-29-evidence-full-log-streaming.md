# Evidence Full-Log Streaming Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Scan every byte of logcat/stdout/stderr for failure signatures while keeping evidence output and memory bounded.

**Architecture:** Add a single-pass line scanner in `runtime/internal/evidence/stream.go`. It groups compiled signatures by source file, keeps a shared 50-line history ring, finishes pending 50-line post-match captures, and collects fallback excerpts during the same read. `evidence.go` remains responsible for contract-facing result assembly and applies the existing 96KiB budget in declaration order after scanning.

**Tech Stack:** Go 1.22+, `bufio.Reader`, Go `regexp`, table-driven tests, JSON Schema draft 2020-12.

---

### Task 1: Pin Full-File Matching and Global Line Numbers

**Files:**
- Modify: `runtime/internal/evidence/evidence_test.go`
- Create: `runtime/internal/evidence/stream_test.go`
- Test: `runtime/internal/evidence/evidence_test.go`
- Test: `runtime/internal/evidence/stream_test.go`

- [ ] **Step 1: Replace the old tail-window truncation test with a failing full-file regression**

Replace `TestExtractFileTruncation` with:

```go
func TestExtractScansWholeLargeFile(t *testing.T) {
	var b strings.Builder
	b.WriteString("HEAD-MARKER fatal\n")
	line := strings.Repeat("p", 199) + "\n"
	lineCount := 1
	for b.Len() < 9<<20 {
		b.WriteString(line)
		lineCount++
	}
	b.WriteString("TAIL-MARKER fatal\n")
	lineCount++

	raw := b.String()
	in := baseInput()
	in.Signatures = []Signature{
		{ID: "head", Where: "logcat", Pattern: "HEAD-MARKER", Classify: "CODE"},
		{ID: "tail", Where: "logcat", Pattern: "TAIL-MARKER", Classify: "CODE"},
	}
	in.Files["logcat"] = strings.NewReader(raw)
	ev := Extract(in)

	if !ev.Signatures[0].Matched {
		t.Fatalf("8MB 以前的头部签名必须命中: %+v", ev.Signatures[0])
	}
	if got := ev.Signatures[0].Matches[0].LineNo; got != 1 {
		t.Errorf("头部 line_no = %d, want 1", got)
	}
	if !ev.Signatures[1].Matched {
		t.Fatalf("尾部签名必须命中: %+v", ev.Signatures[1])
	}
	if got := ev.Signatures[1].Matches[0].LineNo; got != lineCount {
		t.Errorf("尾部 line_no = %d, want %d", got, lineCount)
	}
	if len(ev.Inputs.TruncatedFiles) != 0 {
		t.Errorf("普通大文件已完整扫描,不应标 truncated_files: %v", ev.Inputs.TruncatedFiles)
	}
	for _, a := range ev.Inputs.Attachments {
		if a.Name == "logcat.txt" && a.Size != int64(len(raw)) {
			t.Errorf("attachment size = %d, want full size %d", a.Size, len(raw))
		}
	}
}
```

- [ ] **Step 2: Run the regression and verify RED**

Run:

```bash
cd runtime
PATH=$HOME/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache \
  go test ./internal/evidence -run TestExtractScansWholeLargeFile -count=1 -v
```

Expected: FAIL because `HEAD-MARKER` is outside the retained 8MB tail window.

- [ ] **Step 3: Add a context-boundary regression**

Add:

```go
func TestExtractStreamingContextAcrossReadBoundaries(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 140; i++ {
		if i == 70 {
			fmt.Fprintln(&b, "MATCH-HERE")
		} else {
			fmt.Fprintf(&b, "line-%04d-%s\n", i, strings.Repeat("x", 1024))
		}
	}
	in := baseInput()
	in.Signatures = []Signature{
		{ID: "boundary", Where: "stdout", Pattern: "MATCH-HERE", Classify: "CODE"},
	}
	in.Files["stdout"] = strings.NewReader(b.String())
	ev := Extract(in)

	m := ev.Signatures[0].Matches[0]
	lines := strings.Split(m.Context, "\n")
	if m.LineNo != 70 {
		t.Fatalf("line_no = %d, want 70", m.LineNo)
	}
	if len(lines) != 101 {
		t.Fatalf("context lines = %d, want 101", len(lines))
	}
	if !strings.HasPrefix(lines[0], "line-0020-") ||
		!strings.HasPrefix(lines[100], "line-0120-") {
		t.Errorf("context range = %q ... %q", lines[0], lines[100])
	}
}
```

Also add the contract-facing oversized-line assertion:

```go
func TestExtractOversizedLineMarksFileTruncated(t *testing.T) {
	raw := strings.Repeat("x", (1<<20)+1) + "\nMATCH\n"
	in := baseInput()
	in.Signatures = []Signature{
		{ID: "s", Where: "stderr", Pattern: "MATCH", Classify: "CODE"},
	}
	in.Files["stderr"] = strings.NewReader(raw)
	ev := Extract(in)

	if !ev.Truncated {
		t.Fatal("顶层 truncated 应为 true")
	}
	if got := ev.Inputs.TruncatedFiles; len(got) != 1 || got[0] != "stderr.log" {
		t.Fatalf("truncated_files = %v", got)
	}
	if !ev.Signatures[0].Matched {
		t.Fatalf("超长行后的签名仍应命中: %+v", ev.Signatures[0])
	}
}
```

Add a shared-reader assertion:

```go
func TestExtractMultipleSignaturesShareOneStreamingRead(t *testing.T) {
	in := baseInput()
	in.Signatures = []Signature{
		{ID: "first", Where: "stdout", Pattern: "FIRST", Classify: "CODE"},
		{ID: "second", Where: "stdout", Pattern: "SECOND", Classify: "INFRA"},
	}
	in.Files["stdout"] = strings.NewReader("FIRST\nmiddle\nSECOND\n")
	ev := Extract(in)

	if !ev.Signatures[0].Matched || !ev.Signatures[1].Matched {
		t.Fatalf("同一不可回退 Reader 上的两个签名都必须命中: %+v", ev.Signatures)
	}
}
```

- [ ] **Step 4: Add scanner robustness tests before the scanner exists**

Create `runtime/internal/evidence/stream_test.go`:

```go
package evidence

import (
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
)

func TestScanStreamConsumesOversizedLineAndContinues(t *testing.T) {
	raw := strings.Repeat("x", maxScanLineBytes+4096) + "\nMATCH-AFTER\n"
	scan := scanStream(strings.NewReader(raw), []streamMatcher{{
		index: 0, re: regexp.MustCompile("MATCH-AFTER"),
	}}, false)

	if scan.readErr != nil {
		t.Fatalf("readErr = %v", scan.readErr)
	}
	if !scan.truncated {
		t.Fatal("超长单行必须标记 truncated")
	}
	if scan.size != int64(len(raw)) {
		t.Errorf("size = %d, want %d", scan.size, len(raw))
	}
	got := scan.candidates[0]
	if len(got) != 1 || got[0].lineNo != 2 {
		t.Fatalf("后续命中 = %+v, want line 2", got)
	}
}

type dataAndErrorReader struct {
	done bool
}

func (r *dataAndErrorReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	n := copy(p, "MATCH-BEFORE-ERROR\n")
	return n, errors.New("injected read failure")
}

func TestScanStreamConsumesBytesReturnedWithError(t *testing.T) {
	scan := scanStream(&dataAndErrorReader{}, []streamMatcher{{
		index: 0, re: regexp.MustCompile("MATCH-BEFORE-ERROR"),
	}}, false)
	if scan.readErr == nil || !strings.Contains(scan.readErr.Error(), "injected") {
		t.Fatalf("readErr = %v", scan.readErr)
	}
	if got := scan.candidates[0]; len(got) != 1 || got[0].lineNo != 1 {
		t.Fatalf("错误同批返回的字节未扫描: %+v", got)
	}
}

func TestScanStreamRetainedStateDoesNotGrowWithInput(t *testing.T) {
	raw := strings.Repeat("padding\n", 2_000_000)
	scan := scanStream(strings.NewReader(raw), nil, false)
	if scan.size != int64(len(raw)) {
		t.Fatalf("size = %d, want %d", scan.size, len(raw))
	}
	retained := 0
	for _, line := range scan.tail {
		retained += len(line)
	}
	if retained > excerptFileBytes {
		t.Fatalf("retained tail = %d, want <= %d", retained, excerptFileBytes)
	}
	if len(scan.candidates) != 0 || len(scan.errorLines) != 0 {
		t.Fatalf("无签名普通日志不应保留额外状态: %+v", scan)
	}
}
```

- [ ] **Step 5: Run all new tests and retain the failing evidence**

Run:

```bash
cd runtime
PATH=$HOME/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache \
  go test ./internal/evidence \
  -run 'TestExtractScansWholeLargeFile|TestExtractStreamingContextAcrossReadBoundaries|TestScanStream' \
  -count=1 -v
```

Expected: build failure because `scanStream`, `streamMatcher`, and
`maxScanLineBytes` do not exist yet; the large-file Extract regression would also fail
under the old tail-window implementation.

- [ ] **Step 6: Commit the red tests**

```bash
git add runtime/internal/evidence/evidence_test.go \
  runtime/internal/evidence/stream_test.go
git commit -m "test(evidence): require full-log signature scanning"
```

### Task 2: Implement the Single-Pass Signature Scanner

**Files:**
- Create: `runtime/internal/evidence/stream.go`
- Modify: `runtime/internal/evidence/evidence.go`
- Test: `runtime/internal/evidence/evidence_test.go`

- [ ] **Step 1: Create the scanner data model**

Create `runtime/internal/evidence/stream.go` with these constants and types:

```go
package evidence

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"regexp"
	"strings"
)

const (
	maxScanLineBytes    = 1 << 20
	maxContextLineBytes = 8 << 10
	streamBufferBytes   = 64 << 10
)

type streamMatcher struct {
	index int
	re    *regexp.Regexp
}

type contextLine struct {
	text      string
	truncated bool
}

type matchCandidate struct {
	signatureIndex int
	lineNo         int
	lines          []contextLine
	remaining      int
	truncated      bool
}

func (m matchCandidate) context() string {
	lines := make([]string, len(m.lines))
	for i := range m.lines {
		lines[i] = m.lines[i].text
	}
	return strings.Join(lines, "\n")
}

type streamScan struct {
	size       int64
	lineCount  int
	candidates map[int][]matchCandidate
	overflow   map[int]bool
	tail       []string
	errorLines []string
	truncated  bool
	readErr    error
}
```

- [ ] **Step 2: Implement bounded context-line rendering**

Add:

```go
func contextLineFor(raw []byte, overlong bool) contextLine {
	if len(raw) <= maxContextLineBytes {
		return contextLine{text: string(raw), truncated: overlong}
	}
	const marker = "...<line truncated>..."
	keep := (maxContextLineBytes - len(marker)) / 2
	text := string(raw[:keep]) + marker + string(raw[len(raw)-keep:])
	return contextLine{text: text, truncated: true}
}

func trimLineEnding(raw []byte) []byte {
	raw = bytes.TrimSuffix(raw, []byte{'\n'})
	raw = bytes.TrimSuffix(raw, []byte{'\r'})
	return raw
}
```

The match buffer is allowed to retain up to `maxScanLineBytes`; the context representation
is separately capped at `maxContextLineBytes`.

- [ ] **Step 3: Implement a fragment-consuming bounded line reader**

Add a helper with this exact contract:

```go
func readStreamLine(br *bufio.Reader) (
	matchLine []byte,
	context contextLine,
	size int64,
	overlong bool,
	err error,
) {
	var scan []byte
	var first []byte
	var last []byte
	sawData := false

	for {
		frag, readErr := br.ReadSlice('\n')
		if len(frag) > 0 {
			sawData = true
			size += int64(len(frag))
			if len(scan) < maxScanLineBytes {
				n := maxScanLineBytes - len(scan)
				if n > len(frag) {
					n = len(frag)
				}
				scan = append(scan, frag[:n]...)
			}
			if len(first) < maxContextLineBytes/2 {
				n := maxContextLineBytes/2 - len(first)
				if n > len(frag) {
					n = len(frag)
				}
				first = append(first, frag[:n]...)
			}
			last = append(last, frag...)
			if len(last) > maxContextLineBytes/2 {
				last = append([]byte(nil), last[len(last)-maxContextLineBytes/2:]...)
			}
		}

		switch {
		case readErr == nil:
			matchLine = trimLineEnding(scan)
			overlong = size > int64(maxScanLineBytes)
			if overlong {
				raw := append(append([]byte(nil), first...), last...)
				context = contextLineFor(trimLineEnding(raw), true)
			} else {
				context = contextLineFor(matchLine, false)
			}
			return matchLine, context, size, overlong, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF):
			if !sawData {
				return nil, contextLine{}, 0, false, io.EOF
			}
			matchLine = trimLineEnding(scan)
			overlong = size > int64(maxScanLineBytes)
			if overlong {
				raw := append(append([]byte(nil), first...), last...)
				context = contextLineFor(trimLineEnding(raw), true)
			} else {
				context = contextLineFor(matchLine, false)
			}
			return matchLine, context, size, overlong, nil
		default:
			if sawData {
				matchLine = trimLineEnding(scan)
				context = contextLineFor(matchLine, false)
			}
			return matchLine, context, size, false, readErr
		}
	}
}
```

- [ ] **Step 4: Implement the scanner state machine**

Add:

```go
func scanStream(r io.Reader, matchers []streamMatcher, logcat bool) *streamScan {
	out := &streamScan{
		candidates: make(map[int][]matchCandidate),
		overflow:   make(map[int]bool),
	}
	br := bufio.NewReaderSize(r, streamBufferBytes)
	history := make([]contextLine, 0, contextLines)
	active := make([]matchCandidate, 0)

	finish := func(candidate matchCandidate) {
		candidate.remaining = 0
		out.candidates[candidate.signatureIndex] =
			append(out.candidates[candidate.signatureIndex], candidate)
	}

	for {
		matchLine, ctxLine, n, overlong, err := readStreamLine(br)
		out.size += n
		if errors.Is(err, io.EOF) && n == 0 {
			break
		}
		if n == 0 && err != nil {
			out.readErr = err
			break
		}

		out.lineCount++
		if overlong {
			out.truncated = true
		}

		nextActive := active[:0]
		for i := range active {
			active[i].lines = append(active[i].lines, ctxLine)
			active[i].truncated = active[i].truncated || ctxLine.truncated
			active[i].remaining--
			if active[i].remaining == 0 {
				finish(active[i])
			} else {
				nextActive = append(nextActive, active[i])
			}
		}
		active = nextActive

		for _, matcher := range matchers {
			count := len(out.candidates[matcher.index])
			for _, c := range active {
				if c.signatureIndex == matcher.index {
					count++
				}
			}
			if !matcher.re.Match(matchLine) {
				continue
			}
			if count >= maxMatchesPerSignature {
				out.overflow[matcher.index] = true
				continue
			}
			lines := append([]contextLine(nil), history...)
			lines = append(lines, ctxLine)
			active = append(active, matchCandidate{
				signatureIndex: matcher.index,
				lineNo:         out.lineCount,
				lines:          lines,
				remaining:      contextLines,
				truncated:      ctxLine.truncated,
			})
		}

		if logcat && logcatErrLine.Match(matchLine) &&
			len(out.errorLines) < excerptLogcatMaxLines {
			out.errorLines = append(out.errorLines, ctxLine.text)
		}
		out.tail = append(out.tail, ctxLine.text)
		_, tailTruncated := tailLines(out.tail, excerptFileBytes)
		for tailTruncated && len(out.tail) > 0 {
			out.tail = out.tail[1:]
			_, tailTruncated = tailLines(out.tail, excerptFileBytes)
		}

		history = append(history, ctxLine)
		if len(history) > contextLines {
			history = history[len(history)-contextLines:]
		}
		if err != nil {
			out.readErr = err
			break
		}
	}
	for _, candidate := range active {
		finish(candidate)
	}
	return out
}
```

- [ ] **Step 5: Replace `fileWindow` loading in `Extract`**

In `runtime/internal/evidence/evidence.go`:

1. Remove `maxFileBytes`, `fileWindow`, and `readWindow`.
2. Compile signatures first and store compiled regexes by input index.
3. Build `map[string][]streamMatcher`.
4. Scan each present log key once in fixed `logcat`, `stdout`, `stderr` order.
5. Assemble `SignatureResult` in original declaration order from `streamScan.candidates`.
6. Apply `contextBudgetBytes` after scanning, using `candidate.context()`.
7. Mark the last retained match truncated when the scanner reports overflow or the
   candidate contains clipped context lines.

Use these assembly rules:

```go
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
```

If `scan.readErr != nil` and the signature has no candidates, set
`res.Error = "read <where>: <error>"`. If it has candidates, preserve them, mark the last
one truncated, and set top-level `ev.Truncated = true`.

- [ ] **Step 6: Run focused tests and verify GREEN**

Run:

```bash
cd runtime
PATH=$HOME/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache \
  go test ./internal/evidence \
  -run 'TestExtractSignatures|TestExtractScansWholeLargeFile|TestExtractStreamingContextAcrossReadBoundaries|TestExtractOversizedLine|TestScanStream|TestExtractContextBudget' \
  -count=1 -v
```

Expected: PASS.

- [ ] **Step 7: Run formatting and package tests**

```bash
$HOME/.local/go/bin/gofmt -w \
  runtime/internal/evidence/stream.go \
  runtime/internal/evidence/evidence.go \
  runtime/internal/evidence/evidence_test.go
cd runtime
PATH=$HOME/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache \
  go test ./internal/evidence -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit the streaming scanner**

```bash
git add runtime/internal/evidence/stream.go \
  runtime/internal/evidence/evidence.go \
  runtime/internal/evidence/evidence_test.go
git commit -m "feat(evidence): scan complete logs for signatures"
```

### Task 3: Preserve Fallback Excerpts and File Metadata

**Files:**
- Modify: `runtime/internal/evidence/evidence.go`
- Modify: `runtime/internal/evidence/stream.go`
- Modify: `runtime/internal/evidence/evidence_test.go`

- [ ] **Step 1: Add failing large-file fallback tests**

Add:

```go
func TestExtractFallbackUsesWholeLargeLog(t *testing.T) {
	var logcat strings.Builder
	logcat.WriteString("01-01 00:00:00.0 1 1 E tag: FIRST-ERROR\n")
	for logcat.Len() < 9<<20 {
		logcat.WriteString("01-01 00:00:00.0 1 1 D tag: padding\n")
	}
	in := baseInput()
	in.Signatures = []Signature{
		{ID: "never", Where: "logcat", Pattern: "DOES-NOT-EXIST", Classify: "CODE"},
	}
	in.Files["logcat"] = strings.NewReader(logcat.String())
	ev := Extract(in)

	if len(ev.Excerpts) != 1 {
		t.Fatalf("excerpts = %+v", ev.Excerpts)
	}
	if !strings.Contains(ev.Excerpts[0].Content, "FIRST-ERROR") {
		t.Errorf("全文件最早错误行应保留: %+v", ev.Excerpts[0])
	}
}

func TestExtractLargeStdoutFallbackKeepsTail(t *testing.T) {
	var stdout strings.Builder
	for stdout.Len() < 9<<20 {
		stdout.WriteString("padding\n")
	}
	stdout.WriteString("FINAL-DIAGNOSTIC\n")
	in := baseInput()
	in.Signatures = []Signature{
		{ID: "never", Where: "stdout", Pattern: "DOES-NOT-EXIST", Classify: "CODE"},
	}
	in.Files["stdout"] = strings.NewReader(stdout.String())
	ev := Extract(in)

	if len(ev.Excerpts) != 1 ||
		!strings.Contains(ev.Excerpts[0].Content, "FINAL-DIAGNOSTIC") {
		t.Fatalf("stdout tail = %+v", ev.Excerpts)
	}
}
```

- [ ] **Step 2: Run tests and verify RED**

Run:

```bash
cd runtime
PATH=$HOME/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache \
  go test ./internal/evidence \
  -run 'TestExtractFallbackUsesWholeLargeLog|TestExtractLargeStdoutFallbackKeepsTail' \
  -count=1 -v
```

Expected: at least the logcat test fails until `buildExcerpts` consumes scanner state.

- [ ] **Step 3: Replace `buildExcerpts(load)` with `buildStreamExcerpts(scans)`**

Use:

```go
func buildStreamExcerpts(scans map[string]*streamScan) []Excerpt {
	out := make([]Excerpt, 0, 3)
	budget := contextBudgetBytes

	addTail := func(key, name string) {
		scan := scans[key]
		if scan == nil || len(scan.tail) == 0 || budget <= 0 {
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
			File: name, Kind: "tail", Content: content, Truncated: truncated,
		})
		budget -= len(content)
	}

	addTail("stdout", "stdout.log")
	addTail("stderr", "stderr.log")
	if scan := scans["logcat"]; scan != nil && len(scan.errorLines) > 0 && budget > 0 {
		limit := excerptFileBytes
		if budget < limit {
			limit = budget
		}
		content, truncated := headLines(scan.errorLines, limit)
		if content != "" {
			out = append(out, Excerpt{
				File: "logcat.txt", Kind: "error_lines", Content: content,
				Truncated: truncated || len(scan.errorLines) >= excerptLogcatMaxLines,
			})
		}
	}
	return out
}
```

Call it only when `totalMatched == 0`.

- [ ] **Step 4: Assemble attachments from complete scan metadata**

For log files, use `streamScan.size` as the full byte size. Append a filename to
`TruncatedFiles` only when `streamScan.truncated` is true. Preserve the existing JUnit
placeholder attachment behavior.

Use fixed `fileKeys` order so JSON output remains deterministic.

- [ ] **Step 5: Run fallback and metadata tests**

```bash
cd runtime
PATH=$HOME/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache \
  go test ./internal/evidence \
  -run 'TestExtractFallback|TestExtractLarge|TestExtractScansWholeLargeFile' \
  -count=1 -v
```

Expected: PASS.

- [ ] **Step 6: Commit fallback integration**

```bash
git add runtime/internal/evidence/evidence.go \
  runtime/internal/evidence/stream.go \
  runtime/internal/evidence/evidence_test.go
git commit -m "feat(evidence): retain bounded fallback data during streaming"
```

### Task 4: Version the Evidence Contract and Close Gap #9

**Files:**
- Modify: `contracts/evidence.schema.json`
- Modify: `runtime/internal/evidence/evidence.schema.json`
- Modify: `runtime/internal/evidence/evidence.go`
- Modify: `runtime/internal/evidence/evidence_test.go`
- Modify: `contracts/tests/examples/evidence/valid/full.json`
- Modify: `contracts/tests/examples/evidence/valid/minimal.json`
- Create: `contracts/tests/examples/evidence/invalid/evidence_v2.json`
- Modify: `docs/device-test-sequence.md`
- Modify: `runtime/README.md`

- [ ] **Step 1: Write failing v3 contract assertions**

Create `contracts/tests/examples/evidence/invalid/evidence_v2.json`:

```json
{
  "evidence_version": 2,
  "task_id": "task-old",
  "variant": "aarch64_Android_SNPE_2.21",
  "status": "FAILED",
  "exit_code": 1,
  "cases": {"total": 1, "passed": 0, "failed": 1, "skipped": 0},
  "signatures": [],
  "junit_failures": [],
  "inputs": {"attachments": [], "missing": []}
}
```

Update `TestEvidenceSchemaValidation` to construct `EvidenceVersion: 3`, but do not change
the schemas yet.

- [ ] **Step 2: Run schema tests and verify RED**

```bash
.venv/bin/python -m pytest \
  contracts/tests/test_evidence_schema.py -q
```

Expected: FAIL because the schema still has `const: 2`.

- [ ] **Step 3: Update both schema copies and fixtures**

Apply these exact semantic changes to both schema files:

```json
"title": "evidence.json v3",
"evidence_version": { "const": 3 }
```

Change `inputs.truncated_files.description` to:

```text
提取过程中因超长单行发生扫描截断的文件;普通大文件会完整扫描,不进入此列表
```

Set `evidence_version` to `3` in every evidence valid fixture. Update
`Extract` to emit `EvidenceVersion: 3`.

- [ ] **Step 4: Verify embedded schema equality and contract tests**

```bash
.venv/bin/python -m pytest contracts/tests/test_evidence_schema.py -q
cd runtime
PATH=$HOME/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache \
  go test ./internal/evidence \
  -run 'TestEmbeddedSchemaMatchesContract|TestEvidenceSchemaValidation' \
  -count=1 -v
```

Expected: PASS.

- [ ] **Step 5: Update roadmap and Runtime documentation**

In `docs/device-test-sequence.md`, mark gap #9 implemented with:

```text
**已实现**(2026-07-29):单遍流式扫描完整日志,真实全局行号,
只保留命中 ±50 行/有界 fallback;超长单行显式降级
```

In `runtime/README.md`, add the full-log scanner to the current Phase 2 capability
summary and state that evidence v3 no longer has an 8MB tail-only blind spot.

- [ ] **Step 6: Run complete verification**

Run:

```bash
.venv/bin/python -m pytest

cd agent
PATH=$HOME/.local/go/bin:$PATH \
GOCACHE=/tmp/hermes-agent-go-cache \
GOPATH=/tmp/hermes-agent-gopath \
go test ./...

cd ../runtime
PATH=$HOME/.local/go/bin:$PATH \
GOCACHE=/tmp/hermes-runtime-go-cache \
go test ./...

cd ..
git diff --check
```

Expected:

- Python: all tests pass.
- Agent: all packages pass.
- Runtime: all packages and Temporal spike pass.
- `git diff --check`: no output.

- [ ] **Step 7: Commit contract and documentation**

```bash
git add contracts/evidence.schema.json \
  contracts/tests/examples/evidence/valid/full.json \
  contracts/tests/examples/evidence/valid/minimal.json \
  contracts/tests/examples/evidence/invalid/evidence_v2.json \
  runtime/internal/evidence/evidence.schema.json \
  runtime/internal/evidence/evidence.go \
  runtime/internal/evidence/evidence_test.go \
  docs/device-test-sequence.md \
  runtime/README.md
git commit -m "docs(evidence): publish full-log extractor v3"
```

- [ ] **Step 8: Inspect final branch state**

```bash
git status --short --branch
git log --oneline master..HEAD
```

Expected: clean `evidence-full-log-streaming` branch containing the design, red-test,
scanner, fallback, and contract/documentation commits.
