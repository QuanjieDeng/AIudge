package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// TestModelPaths 覆盖「地址填了根路径，但网关只在 /v1 下挂 /models」这种情况——
// LiteLLM 的报错信息引导的正是 /v1/models，而界面上的地址通常不带 /v1。
func TestModelPaths(t *testing.T) {
	cases := []struct {
		base string
		want []string
	}{
		{"http://gw:4000", []string{"http://gw:4000/models", "http://gw:4000/v1/models"}},
		{"http://gw:4000/", []string{"http://gw:4000/models", "http://gw:4000/v1/models"}},
		{"http://gw:4000/v1", []string{"http://gw:4000/v1/models"}},
		{"http://gw:4000/v1/", []string{"http://gw:4000/v1/models"}},
	}
	for _, c := range cases {
		if got := modelPaths(c.base); !reflect.DeepEqual(got, c.want) {
			t.Errorf("modelPaths(%q) = %v，期望 %v", c.base, got, c.want)
		}
	}
}

// TestModelsFallsBackToV1 网关只实现 /v1/models 时仍要能拉到。
func TestModelsFallsBackToV1(t *testing.T) {
	var hit []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = append(hit, r.URL.Path)
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": "qwen3-max"}, {"id": "deepseek-v3"}},
		})
	}))
	defer srv.Close()

	got, err := New(srv.URL, "sk-x", "", 0).Models(context.Background())
	if err != nil {
		t.Fatalf("应当回退到 /v1/models 成功，得到 %v", err)
	}
	if !reflect.DeepEqual(got, []string{"qwen3-max", "deepseek-v3"}) {
		t.Errorf("模型列表不对: %v", got)
	}
	if len(hit) != 2 || hit[0] != "/models" || hit[1] != "/v1/models" {
		t.Errorf("应当先试 /models 再试 /v1/models，实际请求 %v", hit)
	}
}

// TestModelsRootWorks 根路径可用时不该多打一次无谓的请求。
func TestModelsRootWorks(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "m1"}}})
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "", "", 0).Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("根路径可用时应只请求一次，实际 %d 次", n)
	}
}

// TestModelsBothFail 两条路径都失败时返回错误而不是空列表——
// 空列表会被上层当成「网关没有模型」写进配置。
func TestModelsBothFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(http.NotFound))
	defer srv.Close()

	if _, err := New(srv.URL, "", "", 0).Models(context.Background()); err == nil {
		t.Error("两条路径都 404 时必须返回错误")
	}
}
