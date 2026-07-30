package evidence

import (
	"bufio"
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

const clippedLineMarker = " ... [line clipped] ... "

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

func (c matchCandidate) context() string {
	lines := make([]string, len(c.lines))
	for i, line := range c.lines {
		lines[i] = line.text
	}
	return strings.Join(lines, "\n")
}

type streamScan struct {
	size                int64
	lineCount           int
	candidates          map[int][]matchCandidate
	overflow            map[int]bool
	tail                []contextLine
	errorLines          []string
	errorLinesOverflow  bool
	errorLinesTruncated bool
	truncated           bool
	readErr             error
}

type lineAccumulator struct {
	length int
	prefix []byte
	head   []byte
	tail   []byte
}

func (a *lineAccumulator) append(part []byte) {
	a.length += len(part)

	if len(a.prefix) < maxScanLineBytes {
		n := maxScanLineBytes - len(a.prefix)
		if n > len(part) {
			n = len(part)
		}
		a.prefix = append(a.prefix, part[:n]...)
	}

	if len(a.head) < maxContextLineBytes {
		n := maxContextLineBytes - len(a.head)
		if n > len(part) {
			n = len(part)
		}
		a.head = append(a.head, part[:n]...)
	}

	if len(part) >= maxContextLineBytes {
		a.tail = append(a.tail[:0], part[len(part)-maxContextLineBytes:]...)
		return
	}
	overflow := len(a.tail) + len(part) - maxContextLineBytes
	if overflow > 0 {
		copy(a.tail, a.tail[overflow:])
		a.tail = a.tail[:len(a.tail)-overflow]
	}
	a.tail = append(a.tail, part...)
}

func (a *lineAccumulator) stripTrailingCR() {
	if a.length == 0 || len(a.tail) == 0 || a.tail[len(a.tail)-1] != '\r' {
		return
	}
	a.length--
	a.tail = a.tail[:len(a.tail)-1]
	if a.length < len(a.head) {
		a.head = a.head[:a.length]
	}
	if a.length < len(a.prefix) {
		a.prefix = a.prefix[:a.length]
	}
}

func (a *lineAccumulator) contextLine() contextLine {
	if a.length <= maxContextLineBytes {
		return contextLine{text: string(a.head)}
	}
	available := maxContextLineBytes - len(clippedLineMarker)
	headBytes := available / 2
	tailBytes := available - headBytes
	text := string(a.head[:headBytes]) + clippedLineMarker + string(a.tail[len(a.tail)-tailBytes:])
	return contextLine{text: text, truncated: true}
}

func scanStream(r io.Reader, matchers []streamMatcher, logcat bool) *streamScan {
	out := &streamScan{
		candidates: make(map[int][]matchCandidate),
		overflow:   make(map[int]bool),
	}
	reader := bufio.NewReaderSize(r, streamBufferBytes)
	history := make([]contextLine, 0, contextLines)
	tailBytes := 0
	var line lineAccumulator
	haveLine := false

	processLine := func() {
		line.stripTrailingCR()
		out.lineCount++
		current := line.contextLine()
		if current.truncated {
			out.truncated = true
		}

		matchText := string(line.prefix)
		if logcat && logcatErrLine.MatchString(matchText) {
			if len(out.errorLines) < excerptLogcatMaxLines {
				out.errorLines = append(out.errorLines, current.text)
				out.errorLinesTruncated = out.errorLinesTruncated || current.truncated
			} else {
				out.errorLinesOverflow = true
			}
		}

		for index, candidates := range out.candidates {
			for i := range candidates {
				if candidates[i].remaining == 0 {
					continue
				}
				candidates[i].lines = append(candidates[i].lines, current)
				candidates[i].remaining--
				candidates[i].truncated = candidates[i].truncated || current.truncated
			}
			out.candidates[index] = candidates
		}

		for _, matcher := range matchers {
			if !matcher.re.MatchString(matchText) {
				continue
			}
			candidates := out.candidates[matcher.index]
			if len(candidates) >= maxMatchesPerSignature {
				out.overflow[matcher.index] = true
				continue
			}
			lines := make([]contextLine, 0, len(history)+1+contextLines)
			lines = append(lines, history...)
			lines = append(lines, current)
			truncated := current.truncated
			for _, prior := range history {
				truncated = truncated || prior.truncated
			}
			out.candidates[matcher.index] = append(candidates, matchCandidate{
				signatureIndex: matcher.index,
				lineNo:         out.lineCount,
				lines:          lines,
				remaining:      contextLines,
				truncated:      truncated,
			})
		}

		if len(history) == contextLines {
			copy(history, history[1:])
			history[len(history)-1] = current
		} else {
			history = append(history, current)
		}

		lineBytes := len(current.text) + 1
		out.tail = append(out.tail, current)
		tailBytes += lineBytes
		for tailBytes > excerptFileBytes && len(out.tail) > 0 {
			tailBytes -= len(out.tail[0].text) + 1
			out.tail = out.tail[1:]
		}

		line = lineAccumulator{}
		haveLine = false
	}

	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			out.size += int64(len(fragment))
			haveLine = true
			if fragment[len(fragment)-1] == '\n' {
				line.append(fragment[:len(fragment)-1])
				processLine()
			} else {
				line.append(fragment)
			}
		}

		if err == nil || errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if haveLine {
			processLine()
		}
		if !errors.Is(err, io.EOF) {
			out.readErr = err
		}
		break
	}

	return out
}
