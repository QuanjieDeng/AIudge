// Package llm 是内网 LiteLLM 网关的瘦客户端（§附录决策 14：网关已提供统一协议与计量，
// 不建多模型适配层）。只实现 OpenAI 兼容的 /models 与 /chat/completions 两个端点。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client
}

func New(baseURL, apiKey, model string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    &http.Client{Timeout: timeout},
	}
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type Tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// APIError 是网关返回的业务错误。区分可重试与否：
// 模型名写错、密钥无效、参数非法这类配置问题（4xx）重试多少次结果都一样，
// 只会把一分钟的退避和三条重复日志变成噪音，真正的错因反而被淹没。
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("网关错误 (HTTP %d): %s", e.Status, e.Message)
}

// Retryable 只认 5xx 与 429：前者是网关抽风，后者是限流，都值得等一会儿再来。
func (e *APIError) Retryable() bool {
	return e.Status >= 500 || e.Status == http.StatusTooManyRequests
}

// IsModelError 用来触发「把可用模型列出来」的补充提示。
func (e *APIError) IsModelError() bool {
	return strings.Contains(e.Message, "model") &&
		(strings.Contains(e.Message, "Invalid") || strings.Contains(e.Message, "not found") ||
			strings.Contains(e.Message, "does not exist"))
}

type Reply struct {
	Message Message
	Usage   Usage
	Elapsed time.Duration
}

type chatRequest struct {
	Model          string      `json:"model"`
	Messages       []Message   `json:"messages"`
	Tools          []Tool      `json:"tools,omitempty"`
	ToolChoice     string      `json:"tool_choice,omitempty"`
	Temperature    float64     `json:"temperature"`
	MaxTokens      int         `json:"max_tokens,omitempty"`
	ResponseFormat *respFormat `json:"response_format,omitempty"`
}

type respFormat struct {
	Type string `json:"type"`
}

type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Chat 发一次对话请求。tools 非空时模型可以请求调用工具，由调用方执行后回填。
func (c *Client) Chat(ctx context.Context, msgs []Message, tools []Tool, jsonMode bool) (*Reply, error) {
	req := chatRequest{Model: c.Model, Messages: msgs, Tools: tools, Temperature: 0.1}
	if len(tools) > 0 {
		req.ToolChoice = "auto"
	}
	if jsonMode {
		req.ResponseFormat = &respFormat{Type: "json_object"}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	start := time.Now()
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, fmt.Errorf("网关返回非 JSON (HTTP %d): %s", resp.StatusCode, truncate(string(raw), 300))
	}
	if cr.Error != nil {
		return nil, &APIError{Status: resp.StatusCode, Message: cr.Error.Message}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{Status: resp.StatusCode, Message: truncate(string(raw), 300)}
	}
	if len(cr.Choices) == 0 {
		return nil, fmt.Errorf("网关返回空 choices")
	}
	return &Reply{Message: cr.Choices[0].Message, Usage: cr.Usage, Elapsed: time.Since(start)}, nil
}

// Models 拉取网关可用模型列表（§4.2）。部分网关不实现该端点，此时返回空列表而非报错，
// 让管理员手工填模型名也能继续用。
func (c *Client) Models(ctx context.Context) ([]string, error) {
	var lastErr error
	for _, path := range modelPaths(c.BaseURL) {
		models, err := c.modelsAt(ctx, path)
		if err == nil {
			return models, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// modelPaths 给出候选地址。各家网关对 /v1 前缀的处理不一致：LiteLLM 根路径下也挂
// 一份，但报错信息里引导的是 /v1/models；有的网关只在 /v1 下注册。两个都试一遍，
// 省得让人对着「拉取失败」猜自己是不是少打了三个字符。BaseURL 已带 /v1 时不再重复拼接。
func modelPaths(base string) []string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1") {
		return []string{base + "/models"}
	}
	return []string{base + "/models", base + "/v1/models"}
}

func (c *Client) modelsAt(ctx context.Context, url string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{Status: resp.StatusCode, Message: truncate(string(raw), 200)}
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %v", url, err)
	}
	models := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		models = append(models, m.ID)
	}
	return models, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
