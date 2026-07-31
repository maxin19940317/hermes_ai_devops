package feishucmd

import (
	"errors"
	"testing"
)

// TestRerunReplyPreservesUnderlyingError locks the literal (not merely
// substring) text of the two rerun replies that embed a real error from the
// dependencies, rather than a bare workflow ID:
//
//	WorkflowClosed probe failure  -> "检查 workflow 状态失败: %v"
//	WorkflowResult read failure   -> "读取 workflow 结果失败: %v"
//
// executor_test.go only asserts strings.Contains on these two cases
// (TestRerunExactAuthoritativeContract/RunningOrDescribeErrorRejected and
// /WorkflowResultErrorRejectedWithoutVariant), which would not have caught a
// refactor that swapped the real error for the workflow ID. This file exists
// specifically to pin the byte-for-byte contract without touching that file.
func TestRerunReplyPreservesUnderlyingError(t *testing.T) {
	t.Run("CheckFailed", func(t *testing.T) {
		_, starter, e := newRerunFixture(t)
		starter.closed = false
		starter.closedErr = errors.New("context deadline exceeded")
		got := runRerun(t, e, sourceWorkflowID)
		want := "检查 workflow 状态失败: context deadline exceeded"
		if got != want {
			t.Fatalf("reply = %q, want %q", got, want)
		}
	})

	t.Run("ResultUnreadable", func(t *testing.T) {
		_, starter, e := newRerunFixture(t)
		starter.resultErr = errors.New("result unavailable")
		got := runRerun(t, e, sourceWorkflowID)
		want := "读取 workflow 结果失败: result unavailable"
		if got != want {
			t.Fatalf("reply = %q, want %q", got, want)
		}
	})
}
