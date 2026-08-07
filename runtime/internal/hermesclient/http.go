package hermesclient

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// ErrSchemaInvalid 标记"平台响应不符合契约 Schema"这一类失败,与网络/超时/非 2xx
// 区分开:前者是 prompt 需要迭代的信号,后者是基础设施问题(审计要能分辨)。
var ErrSchemaInvalid = errors.New("hermesclient: 响应不符合契约 Schema")

// 缺省请求超时(Config.Timeout <= 0 时生效)。
const defaultTimeout = 60 * time.Second

// 非 2xx 错误 body 截断长度,防止日志/错误信息被刷爆。
const errBodyLimit = 200

//go:embed analysis.schema.json
var analysisSchemaJSON string

// analysisSchema 是编译期嵌入的 contracts/analysis.schema.json(Draft2020)。
var analysisSchema = mustCompileSchema("analysis.schema.json", analysisSchemaJSON)

//go:embed command.schema.json
var commandSchemaJSON string

// commandSchema 是编译期嵌入的 contracts/command.schema.json(Draft2020)。
var commandSchema = mustCompileSchema("command.schema.json", commandSchemaJSON)

//go:embed plan.schema.json
var planSchemaJSON string

// planSchema 是编译期嵌入的 contracts/plan.schema.json(Draft2020)。
var planSchema = mustCompileSchema("plan.schema.json", planSchemaJSON)

//go:embed express.schema.json
var expressSchemaJSON string

// expressSchema 是编译期嵌入的 contracts/express.schema.json(Draft2020)。
var expressSchema = mustCompileSchema("express.schema.json", expressSchemaJSON)

func mustCompileSchema(name, body string) *jsonschema.Schema {
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	if err := c.AddResource(name, strings.NewReader(body)); err != nil {
		panic(err)
	}
	return c.MustCompile(name)
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
		PromptVersion: PromptVersionAnalyze,
		Model:         req.Model,
		Prompt:        PromptAnalyze,
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

// translatePayload 是发往 bridge POST /translate 的请求格式(§3.3)。
type translatePayload struct {
	PromptVersion string          `json:"prompt_version"`
	Model         string          `json:"model,omitempty"`
	Prompt        string          `json:"prompt"`
	RawText       string          `json:"raw_text"`
	Context       json.RawMessage `json:"context"`
}

// translateURL 由 Endpoint(指向 /analyze)推导出同一 bridge 的 /translate:
// 替换路径最后一段。不新增环境变量,避免两个 URL 被配到不同实例上(设计文档 §7)。
func translateURL(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("hermesclient: Endpoint 不是合法 URL: %w", err)
	}
	p := strings.TrimRight(u.Path, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		u.Path = p[:i] + "/translate"
	} else {
		u.Path = "/translate"
	}
	return u.String(), nil
}

// Translate 调用 bridge 执行一次意图翻译:响应经内嵌 command.schema.json 校验后
// 解析;校验不过或非 2xx 均返回 wrapped error,由调用方回退 usage(设计文档 §6)。
func (c *HTTPClient) Translate(ctx context.Context, req TranslateRequest) (*Translation, error) {
	endpoint, err := translateURL(c.cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	ctxJSON := req.Context
	if len(ctxJSON) == 0 {
		ctxJSON = json.RawMessage(`{}`)
	}
	body, err := json.Marshal(translatePayload{
		PromptVersion: PromptVersionTranslate,
		Model:         req.Model,
		Prompt:        PromptTranslate,
		RawText:       req.RawText,
		Context:       ctxJSON,
	})
	if err != nil {
		return nil, fmt.Errorf("hermesclient: 编码翻译请求失败: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("hermesclient: 构造翻译请求失败: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	if c.cfg.AuthToken != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.cfg.AuthToken)
	}
	resp, err := c.hc.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("hermesclient: 调用 %s 失败: %w", endpoint, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("hermesclient: 读取翻译响应失败: %w", err)
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
		return nil, fmt.Errorf("hermesclient: 翻译响应不是合法 JSON: %w", err)
	}
	if err := commandSchema.Validate(doc); err != nil {
		snippet := string(raw)
		if len(snippet) > errBodyLimit {
			snippet = snippet[:errBodyLimit] + "..."
		}
		return nil, fmt.Errorf("hermesclient: 响应不符合 command.schema.json(视为翻译失败): %w: %w: body=%s", ErrSchemaInvalid, err, snippet)
	}
	var tr Translation
	if err := json.Unmarshal(raw, &tr); err != nil {
		return nil, fmt.Errorf("hermesclient: 解析 Translation 失败: %w", err)
	}
	if err := validateTranslationArgs(tr.Args); err != nil {
		snippet := string(raw)
		if len(snippet) > errBodyLimit {
			snippet = snippet[:errBodyLimit] + "..."
		}
		return nil, fmt.Errorf("hermesclient: 响应不符合 command.schema.json(视为翻译失败): %w: %w: body=%s", ErrSchemaInvalid, err, snippet)
	}
	return &tr, nil
}

func validateTranslationArgs(args []string) error {
	for i, arg := range args {
		if !utf8.ValidString(arg) {
			return fmt.Errorf("args[%d] 不是合法 UTF-8", i)
		}
		runes := utf8.RuneCountInString(arg)
		if runes < 1 || runes > 512 {
			return fmt.Errorf("args[%d] 长度 %d 超出 1..512", i, runes)
		}
		for _, r := range arg {
			if unicode.IsSpace(r) {
				return fmt.Errorf("args[%d] 含 Unicode 空白 U+%04X", i, r)
			}
		}
	}
	return nil
}

// planPayload 是发往 bridge 的规划请求格式。
type planPayload struct {
	PromptVersion string          `json:"prompt_version"`
	Model         string          `json:"model,omitempty"`
	Prompt        string          `json:"prompt"`
	RawText       string          `json:"raw_text"`
	Context       json.RawMessage `json:"context"`
}

// planURL 由 Endpoint(指向 /analyze)推导出同一 bridge 的 /plan:
// 替换路径最后一段。
func planURL(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("hermesclient: Endpoint 不是合法 URL: %w", err)
	}
	p := strings.TrimRight(u.Path, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		u.Path = p[:i] + "/plan"
	} else {
		u.Path = "/plan"
	}
	return u.String(), nil
}

// Plan 调用 bridge 执行一次规划:响应经内嵌 plan.schema.json 校验后
// 返回原始 JSON;校验不过或非 2xx 均返回 wrapped error。
func (c *HTTPClient) Plan(ctx context.Context, req PlanRequest) (json.RawMessage, error) {
	endpoint, err := planURL(c.cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	ctxJSON := req.Context
	if len(ctxJSON) == 0 {
		ctxJSON = json.RawMessage(`{}`)
	}
	body, err := json.Marshal(planPayload{
		PromptVersion: PromptVersionPlan,
		Model:         req.Model,
		Prompt:        PromptPlan,
		RawText:       req.RawText,
		Context:       ctxJSON,
	})
	if err != nil {
		return nil, fmt.Errorf("hermesclient: 编码规划请求失败: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("hermesclient: 构造规划请求失败: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	if c.cfg.AuthToken != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.cfg.AuthToken)
	}
	resp, err := c.hc.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("hermesclient: 调用 %s 失败: %w", endpoint, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("hermesclient: 读取规划响应失败: %w", err)
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
		return nil, fmt.Errorf("hermesclient: 规划响应不是合法 JSON: %w", err)
	}
	if err := planSchema.Validate(doc); err != nil {
		snippet := string(raw)
		if len(snippet) > errBodyLimit {
			snippet = snippet[:errBodyLimit] + "..."
		}
		return nil, fmt.Errorf("hermesclient: 响应不符合 plan.schema.json(视为规划失败): %w: %w: body=%s", ErrSchemaInvalid, err, snippet)
	}
	return raw, nil
}

// expressURL 由 Endpoint(指向 /analyze)推导出同一 bridge 的 /express:
// 替换路径最后一段。与 translateURL/planURL 同构。
func expressURL(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("hermesclient: Endpoint 不是合法 URL: %w", err)
	}
	p := strings.TrimRight(u.Path, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		u.Path = p[:i] + "/express"
	} else {
		u.Path = "/express"
	}
	return u.String(), nil
}

// expressPayload 是发往 bridge 的表述请求格式。Facts 是规则算好的结构化事实,
// Scene 标识场景(命令无关,status 等下一轮平铺复用)。
type expressPayload struct {
	PromptVersion string          `json:"prompt_version"`
	Model         string          `json:"model,omitempty"`
	Prompt        string          `json:"prompt"`
	RawText       string          `json:"raw_text"`
	Scene         string          `json:"scene"`
	Facts         json.RawMessage `json:"facts"`
}

// Express 调用 bridge 执行一次表述:响应经内嵌 express.schema.json 校验后
// 解析;校验不过或非 2xx 均返回 wrapped error,由调用方降级规则文本。
func (c *HTTPClient) Express(ctx context.Context, req ExpressRequest) (*ExpressResponse, error) {
	endpoint, err := expressURL(c.cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	facts := req.Facts
	if len(facts) == 0 {
		facts = json.RawMessage(`{}`)
	}
	body, err := json.Marshal(expressPayload{
		PromptVersion: PromptVersionExpress,
		Model:         req.Model,
		Prompt:        PromptExpress,
		RawText:       req.RawText,
		Scene:         req.Scene,
		Facts:         facts,
	})
	if err != nil {
		return nil, fmt.Errorf("hermesclient: 编码表述请求失败: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("hermesclient: 构造表述请求失败: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	if c.cfg.AuthToken != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.cfg.AuthToken)
	}
	resp, err := c.hc.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("hermesclient: 调用 %s 失败: %w", endpoint, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("hermesclient: 读取表述响应失败: %w", err)
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
		return nil, fmt.Errorf("hermesclient: 表述响应不是合法 JSON: %w", err)
	}
	if err := expressSchema.Validate(doc); err != nil {
		snippet := string(raw)
		if len(snippet) > errBodyLimit {
			snippet = snippet[:errBodyLimit] + "..."
		}
		return nil, fmt.Errorf("hermesclient: 响应不符合 express.schema.json(视为表述失败): %w: %w: body=%s", ErrSchemaInvalid, err, snippet)
	}
	var out ExpressResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("hermesclient: 解析 Express 失败: %w", err)
	}
	return &out, nil
}

var _ Planner = (*HTTPClient)(nil)
var _ Express = (*HTTPClient)(nil)
