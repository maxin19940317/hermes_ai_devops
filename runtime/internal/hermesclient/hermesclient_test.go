package hermesclient

import (
	"bytes"
	"context"
	"encoding/json"
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
				if p.TaskID != "task-1" || p.PromptVersion != PromptVersion ||
					p.Prompt != Prompt || p.RuleCategory != "INFRA" || len(p.Evidence) == 0 {
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
