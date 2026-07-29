package evidence

import (
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
)

func TestScanStreamConsumesOversizedLineAndContinues(t *testing.T) {
	input := strings.Repeat("x", maxScanLineBytes+1) + "\nMATCH-AFTER\n"
	matchers := []streamMatcher{{index: 0, re: regexp.MustCompile("MATCH-AFTER")}}

	got := scanStream(strings.NewReader(input), matchers, false)

	if got.readErr != nil {
		t.Fatalf("readErr = %v", got.readErr)
	}
	if !got.truncated {
		t.Error("oversized line must mark stream truncated")
	}
	if got.size != int64(len(input)) {
		t.Errorf("size = %d, want %d", got.size, len(input))
	}
	if len(got.candidates[0]) != 1 {
		t.Fatalf("candidates = %+v, want one match", got.candidates)
	}
	if got.candidates[0][0].lineNo != 2 {
		t.Errorf("candidate = %+v, want matcher 0 at line 2", got.candidates[0][0])
	}
}

type dataAndErrorReader struct {
	data []byte
	err  error
	off  int
}

func (r *dataAndErrorReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	if r.off == len(r.data) {
		return n, r.err
	}
	return n, nil
}

func TestScanStreamConsumesBytesReturnedWithError(t *testing.T) {
	injected := errors.New("injected read failure")
	input := "MATCH-WITH-ERROR\n"
	matchers := []streamMatcher{{index: 0, re: regexp.MustCompile("MATCH-WITH-ERROR")}}

	got := scanStream(&dataAndErrorReader{data: []byte(input), err: injected}, matchers, false)

	if !errors.Is(got.readErr, injected) {
		t.Fatalf("readErr = %v, want %v", got.readErr, injected)
	}
	if got.size != int64(len(input)) {
		t.Errorf("size = %d, want %d", got.size, len(input))
	}
	if len(got.candidates[0]) != 1 || got.candidates[0][0].lineNo != 1 {
		t.Errorf("candidates = %+v, want line 1 match", got.candidates)
	}
}

func TestScanStreamRetainsOnlyBoundedOutputState(t *testing.T) {
	const lineCount = 10_000
	line := "padding\n"
	input := strings.NewReader(strings.Repeat(line, lineCount))

	got := scanStream(input, nil, false)

	if got.readErr != nil {
		t.Fatalf("readErr = %v", got.readErr)
	}
	if got.size != int64(len(line)*lineCount) {
		t.Errorf("size = %d, want %d", got.size, len(line)*lineCount)
	}
	retainedTailBytes := 0
	for _, line := range got.tail {
		retainedTailBytes += len(line) + 1
	}
	if retainedTailBytes > excerptFileBytes {
		t.Errorf("retained tail = %d bytes, want <= %d", retainedTailBytes, excerptFileBytes)
	}
	if len(got.candidates) != 0 {
		t.Errorf("candidates = %+v, want none", got.candidates)
	}
	if len(got.errorLines) != 0 {
		t.Errorf("errorLines = %+v, want none", got.errorLines)
	}
}
