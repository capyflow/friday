package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capyflow/friday/internal/config"
)

const providerTestSuccess = `{"choices":[{"message":{"role":"assistant","content":"OK"}}]}`

func TestProviderDiagnosticTextAndVisionUseActualRequests(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer secret-test-key" {
			t.Errorf("请求地址/鉴权不符: path=%s", r.URL.Path)
		}
		var request struct {
			Model    string `json:"model"`
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Model != "custom-model" {
			t.Errorf("未使用待测模型: %s", request.Model)
		}
		if count.Add(1) == 2 {
			var content []struct {
				Type     string `json:"type"`
				ImageURL struct {
					URL string `json:"url"`
				} `json:"image_url"`
			}
			if err := json.Unmarshal(request.Messages[0].Content, &content); err != nil || len(content) != 2 || content[1].Type != "image_url" {
				t.Errorf("图片诊断必须真正发送多模态消息，err=%v", err)
			} else {
				imageData, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(content[1].ImageURL.URL, "data:image/png;base64,"))
				if err != nil {
					t.Error(err)
				} else if decoded, err := png.Decode(bytes.NewReader(imageData)); err != nil || decoded.Bounds().Dx() != 1 || decoded.Bounds().Dy() != 1 {
					t.Errorf("测试图片须为有效1x1 PNG，err=%v", err)
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(providerTestSuccess))
	}))
	defer srv.Close()
	result := testProvider(context.Background(), providerTestRequest{BaseURL: srv.URL + "/v1", Model: "custom-model", APIKey: "secret-test-key", CheckVision: true})
	if !result.OK || result.Text.Status != "supported" || result.Vision.Status != "supported" || count.Load() != 2 {
		t.Fatalf("实际文本/图像诊断失败: %+v count=%d", result, count.Load())
	}
	data, _ := json.Marshal(result)
	if bytes.Contains(data, []byte("secret-test-key")) {
		t.Fatal("诊断回显了密钥")
	}
}

func TestProviderDiagnosticClassifiesSafeErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		httpCode   int
		body, code string
	}{
		{"unauthorized", 401, `{"error":{"message":"secret-test-key invalid"}}`, "authentication"},
		{"forbidden", 403, "secret-test-key", "authentication"},
		{"model_not_found", 404, "secret-test-key", "not_found"},
		{"rate_limit", 429, "secret-test-key", "rate_limit"},
		{"service_failure", 503, "secret-test-key", "service"},
		{"incompatible", 400, "secret-test-key", "incompatible"},
		{"invalid_json", 200, "secret-test-key", "invalid_response"},
		{"error_object", 200, `{"error":{"message":"secret-test-key"}}`, "invalid_response"},
		{"empty_response", 200, `{"choices":[{"message":{"content":""}}]}`, "incompatible"},
		{"oversized", 200, strings.Repeat("x", 256*1024+1), "response_size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.httpCode)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			result := testProvider(context.Background(), providerTestRequest{BaseURL: srv.URL, Model: "model", APIKey: "secret-test-key", CheckVision: true})
			if result.OK || result.Text.Code != tc.code || result.Vision.Status != "not_tested" {
				t.Fatalf("错误分类不符: %+v", result)
			}
			data, _ := json.Marshal(result)
			if bytes.Contains(data, []byte("secret-test-key")) {
				t.Fatalf("诊断泄露密钥: %s", data)
			}
		})
	}
}

func TestProviderDiagnosticVisionRejectionAndAmbiguousError(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"error":{"code":"unsupported_image_input","message":"secret-test-key"}}`, "unsupported"},
		{`{"error":{"message":"This model does not support image input"}}`, "unsupported"},
		{`{"error":{"message":"max_tokens is not supported"}}`, "unknown"},
	} {
		var count atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if count.Add(1) == 1 {
				_, _ = w.Write([]byte(providerTestSuccess))
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(tc.body))
		}))
		result := testProvider(context.Background(), providerTestRequest{BaseURL: srv.URL, Model: "model", APIKey: "secret-test-key", CheckVision: true})
		srv.Close()
		if !result.OK || result.Vision.Status != tc.want {
			t.Fatalf("不能凭模型名或无关错误推断图像能力: %+v", result)
		}
	}
}

func TestProviderDiagnosticTimeoutAndRedirectDoNotLeakCredentials(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(true)
		_, _ = w.Write([]byte(providerTestSuccess))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	result := testProvider(context.Background(), providerTestRequest{BaseURL: redirect.URL, Model: "model", APIKey: "secret-test-key"})
	redirect.Close()
	if result.OK || leaked.Load() {
		t.Fatal("重定向不应转发诊断凭据")
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body); <-r.Context().Done() }))
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	result = testProvider(ctx, providerTestRequest{BaseURL: slow.URL, Model: "model", APIKey: "secret-test-key"})
	slow.Close()
	if result.OK || result.Text.Code != "timeout" {
		t.Fatalf("超时分类错误: %+v", result)
	}
}

func TestProviderDiagnosticAuthorizationAndStoredKeyTarget(t *testing.T) {
	var count atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		if r.Header.Get("Authorization") != "Bearer stored-secret" {
			t.Error("同一目标应保留已配置密钥")
		}
		_, _ = w.Write([]byte(providerTestSuccess))
	}))
	defer provider.Close()
	s, _, _ := setupTestServer(t)
	s.cfg = &config.Config{Provider: config.ProviderConfig{BaseURL: provider.URL, Model: "model", APIKey: "stored-secret"}}
	handler := s.Handler()
	body, _ := json.Marshal(providerTestRequest{BaseURL: provider.URL + "/", Model: "model"})
	unauth := setupTestRequest(handler, "POST", "/api/provider/test", body, "127.0.0.1:1234", "localhost", "http://localhost", "")
	if unauth.Code != http.StatusForbidden {
		t.Fatalf("诊断必须登录授权: %d %s", unauth.Code, unauth.Body)
	}
	token := testAuthToken(t, s)
	ok := setupTestRequest(handler, "POST", "/api/provider/test", body, "127.0.0.1:1234", "localhost", "http://localhost", token)
	if ok.Code != http.StatusOK || count.Load() != 1 {
		t.Fatalf("同目标保留密钥诊断失败: %d %s", ok.Code, ok.Body)
	}
	changed, _ := json.Marshal(providerTestRequest{BaseURL: "http://other.example/v1", Model: "model"})
	rejected := setupTestRequest(handler, "POST", "/api/provider/test", changed, "127.0.0.1:1234", "localhost", "http://localhost", token)
	if rejected.Code != http.StatusBadRequest || count.Load() != 1 {
		t.Fatal("不能将现有密钥发往新目标")
	}
	for _, request := range []providerTestRequest{{BaseURL: provider.URL, Model: ""}, {BaseURL: "", Model: "model"}, {BaseURL: "https://user:password@example.com/v1", Model: "model"}} {
		body, _ := json.Marshal(request)
		response := setupTestRequest(handler, "POST", "/api/provider/test", body, "127.0.0.1:1234", "localhost", "http://localhost", token)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("空/非法配置不得测试成功: %d", response.Code)
		}
	}
	if s.cfg.ProviderAPIKey() != "stored-secret" || s.cfg.Snapshot().Provider.BaseURL != provider.URL {
		t.Fatal("诊断修改了生效配置")
	}
}

// TestProviderDiagnosticAnthropic Anthropic 兼容协议：走 {base}/v1/messages，
// 带 x-api-key 与第三方网关需要的 Authorization: Bearer，图片按 image/source 块发。
func TestProviderDiagnosticAnthropic(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/anthropic/v1/messages" {
			t.Errorf("Anthropic 请求路径不符: %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "sk-cp-test" || r.Header.Get("Authorization") != "Bearer sk-cp-test" {
			t.Errorf("鉴权头不符: x-api-key=%q authorization=%q", r.Header.Get("x-api-key"), r.Header.Get("Authorization"))
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("缺少 anthropic-version")
		}
		var req struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			Messages  []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Model != "MiniMax-M3.1-Flash" || req.MaxTokens != 512 {
			t.Errorf("请求体不符: %+v", req)
		}
		if calls.Add(1) == 2 {
			var blocks []struct {
				Type   string `json:"type"`
				Source struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				} `json:"source"`
			}
			if err := json.Unmarshal(req.Messages[0].Content, &blocks); err != nil || len(blocks) != 2 ||
				blocks[1].Type != "image" || blocks[1].Source.Type != "base64" ||
				blocks[1].Source.MediaType != "image/png" || blocks[1].Source.Data == "" {
				t.Errorf("图片诊断须发 Anthropic image 块，err=%v got=%+v", err, blocks)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK"}]}`))
	}))
	defer srv.Close()

	result := testProvider(context.Background(), providerTestRequest{
		BaseURL: srv.URL + "/anthropic", Model: "MiniMax-M3.1-Flash", APIKey: "sk-cp-test",
		Protocol: config.ProtocolAnthropic, CheckVision: true,
	})
	if !result.OK || result.Text.Status != "supported" {
		t.Fatalf("文本诊断应通过: %+v", result.Text)
	}
	if result.Vision.Status != "supported" {
		t.Fatalf("图片诊断应通过: %+v", result.Vision)
	}
}

// TestProviderDiagnosticGemini Gemini 协议：走 {base}/v1beta/models/{model}:generateContent，
// key 在查询参数里，图片按 inlineData 发。
func TestProviderDiagnosticGemini(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/gemini-test:generateContent" {
			t.Errorf("Gemini 请求路径不符: %s", r.URL.Path)
		}
		if r.URL.Query().Get("key") != "gk-test" {
			t.Errorf("key 查询参数不符: %s", r.URL.RawQuery)
		}
		var req struct {
			Contents []struct {
				Parts []struct {
					Text       string `json:"text"`
					InlineData struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"contents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if calls.Add(1) == 2 {
			parts := req.Contents[0].Parts
			if len(parts) != 2 || parts[1].InlineData.MimeType != "image/png" || parts[1].InlineData.Data == "" {
				t.Errorf("图片诊断须发 Gemini inlineData，got=%+v", parts)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"OK"}]}}]}`))
	}))
	defer srv.Close()

	result := testProvider(context.Background(), providerTestRequest{
		BaseURL: srv.URL, Model: "gemini-test", APIKey: "gk-test",
		Protocol: config.ProtocolGemini, CheckVision: true,
	})
	if !result.OK || result.Text.Status != "supported" {
		t.Fatalf("文本诊断应通过: %+v", result.Text)
	}
	if result.Vision.Status != "supported" {
		t.Fatalf("图片诊断应通过: %+v", result.Vision)
	}
}

