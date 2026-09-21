package queue

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"aiudge/internal/llm"
	"aiudge/internal/review"
)

// TestRetryable 区分「值得等一会儿再来」和「再试一百次也一样」。
// 配置类错误重试会把真正的错因埋进重复日志里，是这个判定存在的理由。
func TestRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"模型名写错(400)", &llm.APIError{Status: 400,
			Message: `Invalid model name passed in model=deepseek-flash[1m]. Call /v1/models`}, false},
		{"密钥无效(401)", &llm.APIError{Status: 401, Message: "Invalid API key"}, false},
		{"无权限(403)", &llm.APIError{Status: 403, Message: "forbidden"}, false},
		{"限流(429)", &llm.APIError{Status: http.StatusTooManyRequests, Message: "rate limited"}, true},
		{"网关抽风(500)", &llm.APIError{Status: 500, Message: "internal"}, true},
		{"网关不可达(502)", &llm.APIError{Status: 502, Message: "bad gateway"}, true},
		{"网络错误", errors.New("dial tcp: i/o timeout"), true},
		{"包装过的配置错误", fmt.Errorf("AI 审核失败: %w",
			&llm.APIError{Status: 400, Message: "Invalid model name"}), false},
		// 真实踩过：没配模型时白白退避了 1+2+4 分钟
		{"未配置模型", fmt.Errorf("%w: 未配置模型，请在模型配置里设默认模型", review.ErrConfig), false},
		{"凭据解不开", fmt.Errorf("AI 审核失败: %w: 解密仓库凭据失败", review.ErrConfig), false},
	}
	for _, c := range cases {
		if got := retryable(c.err); got != c.want {
			t.Errorf("%s: retryable=%v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestIsModelError 决定是否值得再去拉一次模型列表补进错误信息。
func TestIsModelError(t *testing.T) {
	yes := []string{
		"Invalid model name passed in model=deepseek-flash[1m]. Call `/v1/models`",
		"model not found",
		"The model `foo` does not exist",
	}
	for _, m := range yes {
		if !(&llm.APIError{Status: 400, Message: m}).IsModelError() {
			t.Errorf("应识别为模型错误: %q", m)
		}
	}
	no := []string{"Invalid API key", "context length exceeded", "rate limit reached"}
	for _, m := range no {
		if (&llm.APIError{Status: 400, Message: m}).IsModelError() {
			t.Errorf("不应识别为模型错误: %q", m)
		}
	}
}
