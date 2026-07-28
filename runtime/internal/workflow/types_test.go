package workflow

import (
	"strings"
	"testing"
)

func TestWorkflowIDBuiltOnBase(t *testing.T) {
	in := DeviceTestInput{Project: "algo-super-sdk", Commit: "9da3b9d9", PipelineID: 56}
	base := BaseWorkflowID(in.Project, in.Commit, in.PipelineID)
	if base != "device-test-algo-super-sdk-g9da3b9d9-p56" {
		t.Fatalf("base = %q", base)
	}
	// bundle 级:ID 就是 base
	if got := in.WorkflowID(); got != base {
		t.Errorf("bundle WorkflowID = %q, want %q", got, base)
	}
	// 变体级与 retry:必须以 base + "-" 开头
	in.Scope, in.Attempt = "aarch64_Android_SNPE_1.68", 2
	if got := in.WorkflowID(); !strings.HasPrefix(got, base+"-") {
		t.Errorf("scoped WorkflowID = %q, want prefix %q", got, base+"-")
	}
}
