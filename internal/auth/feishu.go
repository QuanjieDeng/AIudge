// Package auth 做飞书扫码登录（§附录决策 13）。登录顺带建立「人 ↔ 飞书 open_id」
// 映射，§4.7 的指派通知和 §5 的操作人追溯都依赖它，不需要另外维护人员表。
package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type Feishu struct {
	AppID       string
	AppSecret   string
	RedirectURI string
	BaseURL     string // 私有化部署可换成自建域名
	Client      *http.Client
}

func New(appID, appSecret, redirectURI, baseURL string) *Feishu {
	if baseURL == "" {
		baseURL = "https://open.feishu.cn"
	}
	return &Feishu{
		AppID: appID, AppSecret: appSecret, RedirectURI: redirectURI,
		BaseURL: baseURL, Client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (f *Feishu) Enabled() bool { return f.AppID != "" && f.AppSecret != "" }

type Profile struct {
	OpenID string
	UserID string
	Name   string
	Avatar string
}

// AuthorizeURL 拼扫码页地址。state 由调用方生成并存 cookie，回调时比对防 CSRF。
func (f *Feishu) AuthorizeURL(state string) string {
	q := url.Values{}
	q.Set("client_id", f.AppID)
	q.Set("redirect_uri", f.RedirectURI)
	q.Set("state", state)
	q.Set("response_type", "code")
	q.Set("scope", "contact:user.base:readonly")
	return "https://accounts.feishu.cn/open-apis/authen/v1/authorize?" + q.Encode()
}

// Exchange 用授权码换用户信息。走 v2 的 oauth/token：v1 需要先取 app_access_token，
// 多一次往返且要自己管缓存，v2 一步到位。
func (f *Feishu) Exchange(ctx context.Context, code string) (*Profile, error) {
	if !f.Enabled() {
		return nil, fmt.Errorf("飞书登录未配置")
	}
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     f.AppID,
		"client_secret": f.AppSecret,
		"code":          code,
		"redirect_uri":  f.RedirectURI,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.BaseURL+"/open-apis/authen/v2/oauth/token", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	raw, err := f.do(req)
	if err != nil {
		return nil, fmt.Errorf("换取 token 失败: %w", err)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		Code        int    `json:"code"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("解析 token 响应失败: %v", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("飞书未返回 access_token: %s", truncate(string(raw), 300))
	}

	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, f.BaseURL+"/open-apis/authen/v1/user_info", nil)
	if err != nil {
		return nil, err
	}
	req2.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	raw2, err := f.do(req2)
	if err != nil {
		return nil, fmt.Errorf("拉取用户信息失败: %w", err)
	}
	var info struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			OpenID    string `json:"open_id"`
			UserID    string `json:"user_id"`
			Name      string `json:"name"`
			AvatarURL string `json:"avatar_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw2, &info); err != nil {
		return nil, fmt.Errorf("解析用户信息失败: %v", err)
	}
	if info.Code != 0 {
		return nil, fmt.Errorf("飞书返回错误 %d: %s", info.Code, info.Msg)
	}
	id := info.Data.OpenID
	if id == "" {
		id = info.Data.UserID
	}
	if id == "" {
		return nil, fmt.Errorf("飞书未返回用户标识")
	}
	return &Profile{OpenID: id, UserID: info.Data.UserID, Name: info.Data.Name, Avatar: info.Data.AvatarURL}, nil
}

func (f *Feishu) do(req *http.Request) ([]byte, error) {
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return raw, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
