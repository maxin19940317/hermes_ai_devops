package feishucmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"hermes-devops/runtime/internal/hermesclient"
	"hermes-devops/runtime/internal/store"
)

// fakeTranslator 记录调用次数,便于断言"某些路径下零 LLM 调用"。
type fakeTranslator struct {
	out        *hermesclient.Translation
	err        error
	calls      int
	gotCtxJSON string
}

func (f *fakeTranslator) Translate(_ context.Context, req hermesclient.TranslateRequest) (*hermesclient.Translation, error) {
	f.calls++
	f.gotCtxJSON = string(req.Context)
	return f.out, f.err
}

func newTranslator(f *fakeTranslator, st Store) *Translator {
	return &Translator{
		Client:   f,
		Store:    st,
		Variants: []string{"aarch64_Android_SNPE_1.68", "aarch64_Android_SNPE_2.21"},
		Now:      func() time.Time { return time.Date(2026, 7, 28, 9, 12, 0, 0, time.UTC) },
	}
}

// TestRenderParseRoundTrip 是方案 1 封闭性的核心断言:schema 允许的任何输出
// 渲染成一行文本后,Parse 必须无损切回同一条指令。
func TestRenderParseRoundTrip(t *testing.T) {
	cases := []struct {
		cmd  string
		args []string
	}{
		{"status", nil},
		{"devices", []string{}},
		{"rerun", []string{"9da3b9d9", "56"}},
		{"rerun", []string{"9da3b9d9", "56", "aarch64_Android_SNPE_1.68"}},
		{"unquarantine", []string{"dev-1"}},
		{"unquarantine", []string{"a.b_c-d"}},
	}
	for _, c := range cases {
		line := render(c.cmd, c.args)
		got := Parse(line)
		if got.Name != c.cmd {
			t.Errorf("render+Parse(%q,%v) name = %q, want %q", c.cmd, c.args, got.Name, c.cmd)
		}
		// 逐项比较(而非拼接后比较字符串):否则若 render 误用 "," 拼接参数,
		// Parse 会把多个参数折叠回一个 token,拼接后的字符串仍可能相等,
		// 从而掩盖参数身份被破坏的事实。
		if !slices.Equal(got.Args, c.args) {
			t.Errorf("render+Parse(%q,%v) args = %v, want %v", c.cmd, c.args, got.Args, c.args)
		}
	}
}

func TestTranslateReadOnlyCommandExecutesDirectly(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "devices", Confidence: 0.95, Reason: "询问设备状态",
	}}
	st := store.NewMemStore()
	res := newTranslator(f, st).Translate(context.Background(), "ou_1", "看下设备状态")
	if !res.OK || res.NeedsConfirm {
		t.Fatalf("res = %+v, want OK 且不需确认", res)
	}
	if res.Rendered != "devices" || res.Outcome != store.OutcomeExecuted {
		t.Errorf("rendered=%q outcome=%q", res.Rendered, res.Outcome)
	}
}

func TestTranslateSideEffectCommandNeedsConfirm(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "rerun",
		Args: []string{"9da3b9d9", "56", "aarch64_Android_SNPE_1.68"}, Confidence: 0.92,
	}}
	res := newTranslator(f, store.NewMemStore()).Translate(context.Background(), "ou_1", "重跑昨天那个")
	if !res.OK || !res.NeedsConfirm {
		t.Fatalf("res = %+v, want OK 且需确认", res)
	}
	if res.Outcome != store.OutcomePendingConfirm {
		t.Errorf("outcome = %q", res.Outcome)
	}
}

func TestTranslateRejectsUnknownVariant(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "rerun",
		Args: []string{"9da3b9d9", "56", "aarch64_Android_RKNN_9.9"}, Confidence: 0.95,
	}}
	res := newTranslator(f, store.NewMemStore()).Translate(context.Background(), "ou_1", "重跑那个")
	if res.OK || res.Outcome != store.OutcomeRejectedArgs {
		t.Fatalf("res = %+v, want 拒绝且 rejected_args", res)
	}
}

func TestTranslateRejectsBadSHA(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "rerun", Args: []string{"zzz", "56"}, Confidence: 0.95,
	}}
	res := newTranslator(f, store.NewMemStore()).Translate(context.Background(), "ou_1", "重跑")
	if res.OK || res.Outcome != store.OutcomeRejectedArgs {
		t.Fatalf("res = %+v", res)
	}
}

func TestTranslateRejectsBadIID(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "rerun", Args: []string{"9da3b9d9", "0"}, Confidence: 0.95,
	}}
	res := newTranslator(f, store.NewMemStore()).Translate(context.Background(), "ou_1", "重跑")
	if res.OK || res.Outcome != store.OutcomeRejectedArgs {
		t.Fatalf("res = %+v", res)
	}
}

func TestTranslateRejectsLowConfidence(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "devices", Confidence: 0.5,
	}}
	res := newTranslator(f, store.NewMemStore()).Translate(context.Background(), "ou_1", "嗯")
	if res.OK || res.Outcome != store.OutcomeRejectedLowConfidence {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.Reply, "devices") {
		t.Errorf("低置信度回复应带上翻译结果供人工判断: %q", res.Reply)
	}
}

func TestTranslateNone(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "none", Confidence: 0.9, Reason: "与设备测试无关",
	}}
	res := newTranslator(f, store.NewMemStore()).Translate(context.Background(), "ou_1", "今天天气怎么样")
	if res.OK || res.Outcome != store.OutcomeRejectedNone {
		t.Fatalf("res = %+v", res)
	}
}

func TestTranslateClientError(t *testing.T) {
	f := &fakeTranslator{err: errors.New("boom")}
	res := newTranslator(f, store.NewMemStore()).Translate(context.Background(), "ou_1", "随便说点什么")
	if res.OK || res.Outcome != store.OutcomeTranslatorError {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.Reply, "翻译服务暂时不可用") {
		t.Errorf("reply = %q", res.Reply)
	}
}

// TestTranslateSchemaInvalidError 验证"平台答复不符合 command.schema.json"这一类
// 失败与普通的网络/超时/非 2xx 错误在审计上是可区分的(rejected_schema vs
// translator_error):前者是 prompt 需要迭代的信号,不应被 client 错误的处理逻辑吞掉。
func TestTranslateSchemaInvalidError(t *testing.T) {
	f := &fakeTranslator{err: fmt.Errorf("hermesclient: 响应不符合 command.schema.json: %w", hermesclient.ErrSchemaInvalid)}
	res := newTranslator(f, store.NewMemStore()).Translate(context.Background(), "ou_1", "随便说点什么")
	if res.OK || res.Outcome != store.OutcomeRejectedSchema {
		t.Fatalf("res = %+v, want rejected_schema", res)
	}
	if !strings.Contains(res.Reply, "没理解这句话") {
		t.Errorf("schema 失败应回复“没理解”而非“翻译服务暂时不可用”: %q", res.Reply)
	}
}

// TestTranslateRejectsUnknownDevice 验证 unquarantine 的 device_id 存在性
// 按快照 devices 成员判定(设计文档 §5.3),而不只是校验参数个数。
func TestTranslateRejectsUnknownDevice(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "unquarantine", Args: []string{"dev-ghost"}, Confidence: 0.95,
	}}
	st := store.NewMemStore()
	if err := st.UpsertClientDevices(context.Background(), store.Client{ClientID: "c1"},
		[]store.Device{{DeviceID: "dev-1", Serial: "s1", ClientID: "c1"}}); err != nil {
		t.Fatalf("UpsertClientDevices: %v", err)
	}
	res := newTranslator(f, st).Translate(context.Background(), "ou_1", "解除隔离 dev-ghost")
	if res.OK || res.Outcome != store.OutcomeRejectedArgs {
		t.Fatalf("res = %+v, want 拒绝且 rejected_args", res)
	}
}

func TestTranslateSnapshotCarriesNow(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "devices", Confidence: 0.9,
	}}
	newTranslator(f, store.NewMemStore()).Translate(context.Background(), "ou_1", "设备")
	var snap map[string]any
	if err := json.Unmarshal([]byte(f.gotCtxJSON), &snap); err != nil {
		t.Fatalf("snapshot 不是合法 JSON: %v", err)
	}
	if snap["now"] != "2026-07-28T09:12:00Z" {
		t.Errorf("snapshot.now = %v,缺了它 LLM 无法锚定“昨天”", snap["now"])
	}
	for _, k := range []string{"variants", "recent_runs", "devices"} {
		if _, ok := snap[k]; !ok {
			t.Errorf("snapshot 缺字段 %q", k)
		}
	}
}

// erroringSnapshotStore 让 FleetOverview/RecentRuns 失败,其余方法委托给一个
// 真实 MemStore,用来验证 buildSnapshot 查库失败时降级为空快照而不是崩掉
// (设计文档 §6:快照缺失只会让 LLM 返回 none,是安全降级)。
type erroringSnapshotStore struct {
	*store.MemStore
}

func (erroringSnapshotStore) FleetOverview(context.Context) (*store.FleetOverview, error) {
	return nil, errors.New("db down")
}

func (erroringSnapshotStore) RecentRuns(context.Context, int) ([]store.RecentRun, error) {
	return nil, errors.New("db down")
}

func TestTranslateDegradesWhenStoreErrors(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "devices", Confidence: 0.9,
	}}
	st := erroringSnapshotStore{MemStore: store.NewMemStore()}
	tr := newTranslator(f, st)
	res := tr.Translate(context.Background(), "ou_1", "设备状态")
	if !res.OK || res.Outcome != store.OutcomeExecuted {
		t.Fatalf("res = %+v, want OK/executed despite store errors (safe degrade)", res)
	}
	var snap map[string]any
	if err := json.Unmarshal([]byte(f.gotCtxJSON), &snap); err != nil {
		t.Fatalf("snapshot 不是合法 JSON: %v", err)
	}
	if snap["now"] == nil || snap["now"] == "" {
		t.Errorf("即使查库失败,now 字段也必须在: %v", snap)
	}
	if rr, ok := snap["recent_runs"].([]any); !ok || len(rr) != 0 {
		t.Errorf("recent_runs 降级应为空数组: %v", snap["recent_runs"])
	}
	if dv, ok := snap["devices"].([]any); !ok || len(dv) != 0 {
		t.Errorf("devices 降级应为空数组: %v", snap["devices"])
	}
	// 审计仍必须落库,即使快照降级了。
	rows, err := st.ListCommandTranslations(context.Background(), "ou_1", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListCommandTranslations rows=%v err=%v, want 1 row", rows, err)
	}
}

func TestTranslateAuditsEveryOutcome(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "none", Confidence: 0.2,
	}}
	st := store.NewMemStore()
	newTranslator(f, st).Translate(context.Background(), "ou_1", "什么鬼")
	rows, err := st.ListCommandTranslations(context.Background(), "ou_1", 10)
	if err != nil {
		t.Fatalf("ListCommandTranslations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("审计行数 = %d, want 1", len(rows))
	}
	if rows[0].RawText != "什么鬼" || rows[0].ContextDigest == "" {
		t.Errorf("审计行 = %+v,原文与 context_digest 都必须留痕", rows[0])
	}
}
