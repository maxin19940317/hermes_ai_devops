package rules

import "testing"

func TestDecide(t *testing.T) {
	sigCat := map[string]Category{
		"cpu_fallback": CategoryModel,
		"native_crash": CategoryCode,
	}
	cases := []struct {
		name string
		in   Input
		want Decision
	}{
		{
			name: "全绿通过",
			in:   Input{Status: "COMPLETED", ExitCode: 0},
			want: Decision{Verdict: VerdictPassed},
		},
		{
			name: "取消一律 INCONCLUSIVE(§9)",
			in:   Input{Status: "CANCELED"},
			want: Decision{Verdict: VerdictInconclusive, Category: CategoryUnknown},
		},
		{
			name: "Runtime 判定的基础设施故障可机械重试",
			in:   Input{Status: "FAILED", InfraReason: "lease expired"},
			want: Decision{Verdict: VerdictInfraError, Category: CategoryInfra, Retry: true},
		},
		{
			name: "Client 侧 FAILED(下载/部署失败)按 INFRA 重试",
			in:   Input{Status: "FAILED", ExitCode: -1},
			want: Decision{Verdict: VerdictInfraError, Category: CategoryInfra, Retry: true},
		},
		{
			name: "TIMEOUT 无签名 → INFRA_ERROR(§9)",
			in:   Input{Status: "TIMEOUT", ExitCode: -1},
			want: Decision{Verdict: VerdictInfraError, Category: CategoryInfra, Retry: true},
		},
		{
			name: "TIMEOUT 命中签名 → 用更具体类别,不重试(§9)",
			in:   Input{Status: "TIMEOUT", SignaturesHit: []string{"native_crash"}, SignatureCategory: sigCat},
			want: Decision{Verdict: VerdictTestFailed, Category: CategoryCode},
		},
		{
			name: "签名 cpu_fallback → MODEL,不重试",
			in:   Input{Status: "COMPLETED", ExitCode: 0, SignaturesHit: []string{"cpu_fallback"}, SignatureCategory: sigCat},
			want: Decision{Verdict: VerdictTestFailed, Category: CategoryModel},
		},
		{
			name: "未知签名 → UNKNOWN,不重试",
			in:   Input{Status: "COMPLETED", ExitCode: 0, SignaturesHit: []string{"mystery"}, SignatureCategory: sigCat},
			want: Decision{Verdict: VerdictTestFailed, Category: CategoryUnknown},
		},
		{
			name: "用例失败 → CODE,不重试",
			in:   Input{Status: "COMPLETED", ExitCode: 0, CasesFailed: 3},
			want: Decision{Verdict: VerdictTestFailed, Category: CategoryCode},
		},
		{
			name: "非零退出码 → CODE,不重试",
			in:   Input{Status: "COMPLETED", ExitCode: 7},
			want: Decision{Verdict: VerdictTestFailed, Category: CategoryCode},
		},
		{
			name: "签名优先于用例计数(分类更具体)",
			in:   Input{Status: "COMPLETED", ExitCode: 1, CasesFailed: 1, SignaturesHit: []string{"cpu_fallback"}, SignatureCategory: sigCat},
			want: Decision{Verdict: VerdictTestFailed, Category: CategoryModel},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.in)
			if got.Verdict != tc.want.Verdict || got.Category != tc.want.Category || got.Retry != tc.want.Retry {
				t.Errorf("Decide(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
			if got.Reason == "" {
				t.Error("Reason 不得为空(进通知与 decisions 审计)")
			}
		})
	}
}
