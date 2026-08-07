package hermesclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 防契约漂移:包内嵌入副本必须与 contracts/ 本源一致。
func TestEmbeddedSchemaMatchesContract(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "analysis.schema.json"))
	if err != nil {
		t.Fatalf("read contracts schema: %v", err)
	}
	if !bytes.Equal([]byte(analysisSchemaJSON), want) {
		t.Fatal("embedded analysis.schema.json 与 contracts/ 不一致,请重新拷贝(防契约漂移)")
	}
}

const validAnalysis = `{
  "analysis_version": 1,
  "summary": "native crash 于 libmodel.so",
  "root_cause": "证据显示 SIGSEGV,栈顶位于模型推理路径",
  "suggested_category": "CODE",
  "confidence": 0.9,
  "next_actions": ["检查该 commit 的模型输入变更"],
  "disagrees_with_rule": false
}`

func newReq() AnalyzeRequest {
	return AnalyzeRequest{
		TaskID:       "task-1",
		RuleCategory: "INFRA",
		Evidence:     json.RawMessage(`{"evidence_version":1}`),
	}
}

func TestAnalyze(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		timeout time.Duration
		token   string
		wantErr string // 空 = 期望成功;否则错误信息需包含该子串
	}{
		{
			name: "成功返回合法 analysis(空 token 不带 Authorization)",
			handler: func(w http.ResponseWriter, r *http.Request) {
				// 顺带校验请求体规范格式
				var p analyzePayload
				if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
					t.Errorf("请求体不是合法 JSON: %v", err)
				}
				if p.TaskID != "task-1" || p.PromptVersion != PromptVersionAnalyze ||
					p.Prompt != PromptAnalyze || p.RuleCategory != "INFRA" || len(p.Evidence) == 0 {
					t.Errorf("请求体字段不符合规范: %+v", p)
				}
				if p.Model != "" { // 未指定模型时 omitempty 生效
					t.Errorf("model 应为空(omitempty),实际 %q", p.Model)
				}
				if got := r.Header.Get("Authorization"); got != "" {
					t.Errorf("空 token 不应带 Authorization,实际 %q", got)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(validAnalysis))
			},
		},
		{
			name: "非 2xx 返回带状态码错误并截断 body",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(strings.Repeat("x", 500)))
			},
			wantErr: "500",
		},
		{
			name: "非法 JSON 响应",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("not-json"))
			},
			wantErr: "不是合法 JSON",
		},
		{
			name: "schema 不符(confidence>1)",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{
				  "analysis_version": 1,
				  "summary": "x",
				  "suggested_category": "CODE",
				  "confidence": 1.5,
				  "disagrees_with_rule": false
				}`))
			},
			wantErr: "analysis.schema.json",
		},
		{
			name: "schema 不符(额外字段,字段闭合)",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{
				  "analysis_version": 1,
				  "summary": "x",
				  "suggested_category": "CODE",
				  "confidence": 0.5,
				  "disagrees_with_rule": false,
				  "extra_field": true
				}`))
			},
			wantErr: "analysis.schema.json",
		},
		{
			name: "schema 不符(analysis_version!=1)",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{
				  "analysis_version": 2,
				  "summary": "x",
				  "suggested_category": "CODE",
				  "confidence": 0.5,
				  "disagrees_with_rule": false
				}`))
			},
			wantErr: "analysis.schema.json",
		},
		{
			name: "超时(短 Timeout + 服务端睡眠)",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				time.Sleep(500 * time.Millisecond)
				_, _ = w.Write([]byte(validAnalysis))
			},
			timeout: 50 * time.Millisecond,
			wantErr: "hermesclient",
		},
		{
			name: "token 正确携带",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
					t.Errorf("Authorization = %q, want %q", got, "Bearer secret-token")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(validAnalysis))
			},
			token: "secret-token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			cfg := Config{Endpoint: srv.URL, Timeout: tt.timeout, AuthToken: tt.token}
			c := NewHTTPClient(cfg)
			if c == nil {
				t.Fatal("NewHTTPClient 返回 nil(Endpoint 非空)")
			}
			a, err := c.Analyze(context.Background(), newReq())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("期望成功,得到错误: %v", err)
				}
				if a.AnalysisVersion != 1 || a.Summary == "" ||
					a.SuggestedCategory != "CODE" || a.Confidence != 0.9 ||
					len(a.NextActions) != 1 || a.DisagreesWithRule {
					t.Errorf("Analysis 解析不正确: %+v", a)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望错误 %q,得到 nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("错误 %q 不包含 %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestNon2xxErrorBodyTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("y", 1000)))
	}))
	defer srv.Close()
	c := NewHTTPClient(Config{Endpoint: srv.URL})
	_, err := c.Analyze(context.Background(), newReq())
	if err == nil {
		t.Fatal("期望错误")
	}
	// 状态码 + 截断 body(200 字符 + "...")应远小于原始 1000 字符
	if len(err.Error()) > 260 {
		t.Fatalf("错误信息未截断 body: len=%d", len(err.Error()))
	}
}

func TestModelPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p analyzePayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("请求体不是合法 JSON: %v", err)
		}
		if p.Model != "qwen-max" {
			t.Errorf("model 透传失败: %q", p.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(validAnalysis))
	}))
	defer srv.Close()
	c := NewHTTPClient(Config{Endpoint: srv.URL})
	req := newReq()
	req.Model = "qwen-max"
	if _, err := c.Analyze(context.Background(), req); err != nil {
		t.Fatalf("Analyze: %v", err)
	}
}

func TestNewHTTPClientEmptyEndpoint(t *testing.T) {
	if c := NewHTTPClient(Config{}); c != nil {
		t.Fatal("空 Endpoint 应返回 nil(调用方判未启用)")
	}
}

func TestTranslateURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://h:18100/analyze", "http://h:18100/translate"},
		{"http://h:18100/analyze/", "http://h:18100/translate"},
		{"http://h/hermes/analyze", "http://h/hermes/translate"},
	}
	for _, c := range cases {
		got, err := translateURL(c.in)
		if err != nil {
			t.Fatalf("translateURL(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("translateURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTranslateOK(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"translation_version":3,"command":"devices","args":[],"confidence":0.95}`))
	}))
	defer srv.Close()
	c := NewHTTPClient(Config{Endpoint: srv.URL + "/analyze"})
	tr, err := c.Translate(context.Background(), TranslateRequest{
		RawText: "看下设备状态", Context: json.RawMessage(`{"now":"2026-07-28T09:12:00Z"}`),
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if gotPath != "/translate" {
		t.Errorf("path = %q, want /translate", gotPath)
	}
	if tr.Command != "devices" || tr.Confidence != 0.95 {
		t.Errorf("unexpected translation: %+v", tr)
	}
}

func TestTranslateRejectsInvalidSchema(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// command 不在闭枚举内:必须被本地 Schema 校验挡下(跨进程边界不信任对端)
		_, _ = w.Write([]byte(`{"translation_version":3,"command":"reboot","confidence":0.9}`))
	}))
	defer srv.Close()
	c := NewHTTPClient(Config{Endpoint: srv.URL + "/analyze"})
	_, err := c.Translate(context.Background(), TranslateRequest{RawText: "x"})
	if err == nil {
		t.Fatal("want error for schema-invalid response, got nil")
	}
	// Schema 失败必须能与网络/超时/非 2xx 等基础设施错误区分开(审计要能分辨
	// "prompt 需要迭代" vs "桥不可达"),因此必须满足 errors.Is(err, ErrSchemaInvalid)。
	if !errors.Is(err, ErrSchemaInvalid) {
		t.Fatalf("want errors.Is(err, ErrSchemaInvalid), got %v", err)
	}
}

// TestTranslateSchemaInvalidIncludesBodySnippet 验证 §4.3 的 output 截断路径确实
// 可达:schema 校验失败时,wrapped error 必须携带响应原文的一段可辨识片段
// (此前只落 {"error": Go 错误字符串},该字符串不含平台实际返回值,截断逻辑形同虚设)。
func TestTranslateSchemaInvalidIncludesBodySnippet(t *testing.T) {
	const marker = "reboot-xyz-distinctive-marker"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"translation_version":3,"command":"` + marker + `","confidence":0.9}`))
	}))
	defer srv.Close()
	c := NewHTTPClient(Config{Endpoint: srv.URL + "/analyze"})
	_, err := c.Translate(context.Background(), TranslateRequest{RawText: "x"})
	if err == nil {
		t.Fatal("want error for schema-invalid response, got nil")
	}
	if !strings.Contains(err.Error(), marker) {
		t.Fatalf("error should include a snippet of the offending response body, got: %v", err)
	}
	if !errors.Is(err, ErrSchemaInvalid) {
		t.Fatalf("want errors.Is(err, ErrSchemaInvalid), got %v", err)
	}
}

func TestTranslateNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()
	c := NewHTTPClient(Config{Endpoint: srv.URL + "/analyze"})
	_, err := c.Translate(context.Background(), TranslateRequest{RawText: "x"})
	if err == nil {
		t.Fatal("want error for 502, got nil")
	}
	// 非 2xx 是基础设施问题,不应被误判成"prompt 需要迭代"的 schema 失败。
	if errors.Is(err, ErrSchemaInvalid) {
		t.Fatalf("502 不应满足 errors.Is(err, ErrSchemaInvalid): %v", err)
	}
}

// TestCommandSchemaRejectsArgTrailingNewline 是 command.schema.json args pattern
// 的锚点漂移回归测试:Python 的 re 把 "$" 当作也匹配"换行符之前"(不是严格字符串
// 结尾),Go 的 regexp 不会,因此 "9da3b9d9\n" 对 Python 校验器合法、对 Go 校验器
// 非法。companion 的 anchor-free "not" 约束必须让两侧行为一致地拒绝它;这里独立
// 验证 Go 侧(contracts/tests/test_command_schema.py 用同一份 fixture 验证 Python 侧)。
func TestCommandSchemaRejectsArgTrailingNewline(t *testing.T) {
	raw := []byte(`{"translation_version":3,"command":"unquarantine","args":["9da3b9d9\n"],"confidence":0.9}`)
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	if err := commandSchema.Validate(doc); err == nil {
		t.Fatal("want validation error for trailing-newline arg (anchor-free companion pattern should reject it), got nil")
	}
}

func TestTranslateRejectsUnicodeWhitespaceArgs(t *testing.T) {
	for _, whitespace := range []rune{'\u00a0', '\u2003', '\u3000'} {
		t.Run(fmt.Sprintf("U+%04X", whitespace), func(t *testing.T) {
			response, err := json.Marshal(Translation{
				TranslationVersion: 3,
				Command:            "rerun",
				Args:               []string{"workflow" + string(whitespace) + "id"},
				Confidence:         0.9,
			})
			if err != nil {
				t.Fatalf("marshal response: %v", err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(response)
			}))
			defer srv.Close()

			c := NewHTTPClient(Config{Endpoint: srv.URL + "/analyze"})
			_, err = c.Translate(context.Background(), TranslateRequest{RawText: "重跑"})
			if !errors.Is(err, ErrSchemaInvalid) {
				t.Fatalf("Translate arg containing U+%04X error = %v, want ErrSchemaInvalid", whitespace, err)
			}
		})
	}
}

func TestValidateTranslationArgsDefensiveBoundaries(t *testing.T) {
	cases := []struct {
		name string
		arg  string
		ok   bool
	}{
		{name: "valid", arg: "device-test-grp/algo-super-sdk-g9da3b9d9-p56", ok: true},
		{name: "empty", arg: ""},
		{name: "invalid_utf8", arg: string([]byte{0xff})},
		{name: "too_long", arg: strings.Repeat("界", 513)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTranslationArgs([]string{tc.arg})
			if (err == nil) != tc.ok {
				t.Fatalf("validateTranslationArgs(%q) error = %v, want ok=%v", tc.arg, err, tc.ok)
			}
		})
	}
}

func TestCommandSchemaCopyMatchesContracts(t *testing.T) {
	src, err := os.ReadFile("../../../contracts/command.schema.json")
	if err != nil {
		t.Fatalf("read contracts copy: %v", err)
	}
	if string(src) != commandSchemaJSON {
		t.Error("command.schema.json 与 contracts/ 不一致,请同步副本")
	}
}

func TestTranslateUsesV3PromptAndContract(t *testing.T) {
	if PromptVersionTranslate != "cmd_translate_v3" {
		t.Fatalf("PromptVersionTranslate = %q, want cmd_translate_v3", PromptVersionTranslate)
	}
	for _, want := range []string{
		"test <variant> [commit]",
		"rerun <source_workflow_id> [variant]",
		"authoritative:true",
	} {
		if !strings.Contains(PromptTranslate, want) {
			t.Errorf("PromptTranslate 缺少 %q", want)
		}
	}
}
