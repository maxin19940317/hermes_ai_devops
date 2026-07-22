package hermesclient

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// 缺省请求超时(Config.Timeout <= 0 时生效)。
const defaultTimeout = 60 * time.Second

// 非 2xx 错误 body 截断长度,防止日志/错误信息被刷爆。
const errBodyLimit = 200

//go:embed analysis.schema.json
var analysisSchemaJSON string

// analysisSchema 是编译期嵌入的 contracts/analysis.schema.json(Draft2020)。
var analysisSchema = mustCompileAnalysisSchema()

func mustCompileAnalysisSchema() *jsonschema.Schema {
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	if err := c.AddResource("analysis.schema.json", strings.NewReader(analysisSchemaJSON)); err != nil {
		panic(err)
	}
	return c.MustCompile("analysis.schema.json")
}

// Config 是 HTTPClient 的配置。HTTPDoer 可注入 *http.Client 便于测试。
type Config struct {
	Endpoint  string        // hermes-agent 平台的完整调用 URL
	AuthToken string        // 可选;非空时以 Authorization: Bearer 携带
	Timeout   time.Duration // <=0 时缺省 60s
	HTTPDoer  *http.Client  // 可选;为空时用带 Timeout 的缺省 client
}

// HTTPClient 是 Client 的 HTTP 实现。平台确切请求/响应格式未知,适配差异
// 只应改本文件的构造/解析部分,接口与 Schema 校验不受影响。
type HTTPClient struct {
	cfg Config
	hc  *http.Client
}

// NewHTTPClient 构造 HTTPClient。约定:Endpoint 为空返回 nil,由调用方判
// "Analyzer 未启用"(而不是在这里报错),从而跳过分析、直接走规则引擎。
func NewHTTPClient(cfg Config) *HTTPClient {
	if cfg.Endpoint == "" {
		return nil
	}
	hc := cfg.HTTPDoer
	if hc == nil {
		// 未注入 client:自建,Timeout 缺省 60s
		to := cfg.Timeout
		if to <= 0 {
			to = defaultTimeout
		}
		hc = &http.Client{Timeout: to}
	}
	return &HTTPClient{cfg: cfg, hc: hc}
}

// analyzePayload 是发往平台的规范请求格式(平台适配差异只改这里与响应解析)。
type analyzePayload struct {
	TaskID        string          `json:"task_id"`
	PromptVersion string          `json:"prompt_version"`
	Model         string          `json:"model,omitempty"`
	Prompt        string          `json:"prompt"`
	RuleCategory  string          `json:"rule_category"`
	Evidence      json.RawMessage `json:"evidence"`
}

// Analyze 调用平台执行一次分析:POST Endpoint,响应(2xx 时 body 即 analysis JSON)
// 经内嵌 analysis.schema.json 校验后解析;校验不过或非 2xx 均返回 wrapped error,
// 视为 Analyzer 失败,由调用方回退规则引擎。
func (c *HTTPClient) Analyze(ctx context.Context, req AnalyzeRequest) (*Analysis, error) {
	body, err := json.Marshal(analyzePayload{
		TaskID:        req.TaskID,
		PromptVersion: PromptVersion,
		Model:         req.Model,
		Prompt:        Prompt,
		RuleCategory:  req.RuleCategory,
		Evidence:      req.Evidence,
	})
	if err != nil {
		return nil, fmt.Errorf("hermesclient: 编码请求失败: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("hermesclient: 构造请求失败: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	if c.cfg.AuthToken != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.cfg.AuthToken)
	}
	resp, err := c.hc.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("hermesclient: 调用 %s 失败: %w", c.cfg.Endpoint, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("hermesclient: 读取响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet := string(raw)
		if len(snippet) > errBodyLimit {
			snippet = snippet[:errBodyLimit] + "..."
		}
		return nil, fmt.Errorf("hermesclient: 平台返回 %d: %s", resp.StatusCode, snippet)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("hermesclient: 响应不是合法 JSON: %w", err)
	}
	if err := analysisSchema.Validate(doc); err != nil {
		return nil, fmt.Errorf("hermesclient: 响应不符合 analysis.schema.json(视为 Analyzer 失败): %w", err)
	}
	var a Analysis
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("hermesclient: 解析 Analysis 失败: %w", err)
	}
	return &a, nil
}
