package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	vhttp "github.com/capyflow/vortexv3/server/http"
)

func localSetupRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return false
	}
	requestHost := r.Host
	if h, _, err := net.SplitHostPort(requestHost); err == nil {
		requestHost = h
	}
	if !strings.EqualFold(requestHost, "localhost") {
		ip := net.ParseIP(strings.Trim(requestHost, "[]"))
		if ip == nil || !ip.IsLoopback() {
			return false
		}
	}
	return sameOriginRequest(r)
}

func sameOriginRequest(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		return err == nil && u.Scheme == scheme && strings.EqualFold(u.Host, r.Host) && u.User == nil
	}
	return true
}

func (s *Server) setupStatus(r *http.Request) map[string]any {
	accountRequired := s.accounts != nil && s.accounts.Current().SetupRequired
	providerRequired := s.cfg != nil && s.cfg.ProviderAPIKey() == ""
	return map[string]any{"accountRequired": accountRequired, "providerRequired": providerRequired,
		"localSetupAllowed": accountRequired && localSetupRequest(r)}
}

func (s *Server) handleSetupStatus(ctx *vhttp.Context) error {
	return ctx.JsonResponse(vhttp.Codes.Success, s.setupStatus(ctx.GinContext().Request))
}

func (s *Server) handleSetup(ctx *vhttp.Context) error {
	r := ctx.GinContext().Request
	r.Body = http.MaxBytesReader(ctx.GinContext().Writer, r.Body, 64*1024)
	if !localSetupRequest(r) {
		return ctx.JsonResponse(vhttp.Code{Code: http.StatusForbidden, MsgKey: "forbidden"}, map[string]string{"error": "首次设置仅允许本机访问；远程部署请预置 FRIDAY_ADMIN_USERNAME 和 FRIDAY_ADMIN_PASSWORD"})
	}
	if s.accounts == nil || s.accounts.Err() != nil {
		return ctx.JsonResponse(vhttp.Codes.InternalError, map[string]string{"error": "账号存储不可用，请恢复账号文件后重试"})
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	if err := s.accounts.Initialize(req.Username, req.Password); err != nil {
		if errors.Is(err, ErrAlreadyInitialized) {
			return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": ErrAlreadyInitialized.Error()})
		}
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "账号设置失败，请检查用户名、密码和账号目录是否可写"})
	}
	acc := s.accounts.Current()
	out := s.setupStatus(r)
	out["username"] = acc.Username
	// 建号即登录：下发与 /api/login 同形的 JWT，前端保存后直接进主界面。
	if token, err := s.issueToken(acc.Username); err == nil {
		out["token"] = token
	}
	return ctx.JsonResponse(vhttp.Codes.Success, out)
}

type providerTestRequest struct {
	BaseURL     string `json:"baseURL"`
	Model       string `json:"model"`
	APIKey      string `json:"apiKey"`
	CheckVision bool   `json:"checkVision"`
}

type providerCheck struct {
	Status  string `json:"status"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

type providerTestResult struct {
	OK     bool          `json:"ok"`
	Text   providerCheck `json:"text"`
	Vision providerCheck `json:"vision"`
}

func providerTarget(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("API 地址须为有效的 HTTP 或 HTTPS 地址，且不包含凭据或查询参数")
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

func (s *Server) handleProviderTest(ctx *vhttp.Context) error {
	r := ctx.GinContext().Request
	r.Body = http.MaxBytesReader(ctx.GinContext().Writer, r.Body, 64*1024)
	if !sameOriginRequest(ctx.GinContext().Request) {
		return ctx.JsonResponse(vhttp.Code{Code: http.StatusForbidden, MsgKey: "forbidden"}, map[string]string{"error": "请从当前页面发起连接测试"})
	}
	var req providerTestRequest
	if err := ctx.UnmarshalBody(&req); err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请求体须为 JSON 对象"})
	}
	target, err := providerTarget(req.BaseURL)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	req.BaseURL, req.Model, req.APIKey = target, strings.TrimSpace(req.Model), strings.TrimSpace(req.APIKey)
	if req.Model == "" {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "模型名不能为空"})
	}
	if req.APIKey == "" && s.cfg != nil {
		current := s.cfg.Snapshot()
		oldTarget, _ := providerTarget(current.Provider.BaseURL)
		if oldTarget == target {
			req.APIKey = current.ProviderAPIKey()
		}
	}
	if req.APIKey == "" {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": "请填写该 API 地址的 API Key"})
	}
	return ctx.JsonResponse(vhttp.Codes.Success, testProvider(ctx.Context(), req))
}

func testProvider(ctx context.Context, req providerTestRequest) providerTestResult {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse // 不将凭据转发到重定向地址。
	}}
	result := providerTestResult{Text: callProvider(ctx, client, req, false), Vision: providerCheck{Status: "not_tested", Message: "未测试图片理解"}}
	result.OK = result.Text.Status == "supported"
	if req.CheckVision && result.OK {
		result.Vision = callProvider(ctx, client, req, true)
	}
	return result
}

const providerTestImage = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4//8/AAX+Av4N70a4AAAAAElFTkSuQmCC"

func callProvider(ctx context.Context, client *http.Client, req providerTestRequest, vision bool) providerCheck {
	var content any = "Reply with OK. This is a connection test."
	if vision {
		content = []map[string]any{{"type": "text", "text": "This is a one-pixel test image. Reply with OK if you can receive it."},
			{"type": "image_url", "image_url": map[string]string{"url": providerTestImage}}}
	}
	// 思考型模型会先消耗 token 思考再作答，预算太小会得到空 content。
	body, _ := json.Marshal(map[string]any{"model": req.Model, "messages": []map[string]any{{"role": "user", "content": content}}, "max_tokens": 512, "stream": false})
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, req.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return providerCheck{Status: "unknown", Code: "invalid_request", Message: "无法创建模型请求"}
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+req.APIKey)
	response, err := client.Do(r)
	if err != nil {
		code, message := "network", "无法连接模型服务，请检查地址与网络"
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
			code, message = "timeout", "模型服务响应超时，请稍后重试"
		}
		return providerCheck{Status: "unknown", Code: code, Message: message}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 256*1024+1))
	if err != nil || len(data) > 256*1024 {
		return providerCheck{Status: "unknown", Code: "response_size", Message: "模型响应过大或无法读取"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code, message := "incompatible", "服务响应不兼容，请检查 API 地址与模型名"
		switch {
		case response.StatusCode == 401 || response.StatusCode == 403:
			code, message = "authentication", "鉴权失败，请检查 API Key 和访问权限"
		case response.StatusCode == 404:
			code, message = "not_found", "模型或接口不存在，请检查 API 地址与模型名"
		case response.StatusCode == 429:
			code, message = "rate_limit", "额度不足或请求过于频繁，请稍后重试"
		case response.StatusCode >= 500:
			code, message = "service", "模型服务暂时不可用，请稍后重试"
		}
		status := "unknown"
		if vision && (response.StatusCode == 400 || response.StatusCode == 415 || response.StatusCode == 422) && rejectedImageInput(data) {
			status, code, message = "unsupported", "vision_unsupported", "模型未接受图片输入，请使用支持图片的模型"
		}
		return providerCheck{Status: status, Code: code, Message: message}
	}
	var decoded struct {
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Message struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				Reasoning        string          `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil || (len(decoded.Error) > 0 && string(decoded.Error) != "null") || len(decoded.Choices) == 0 {
		return providerCheck{Status: "unknown", Code: "invalid_response", Message: "服务未返回有效的对话响应，请确认使用 OpenAI 兼容接口"}
	}
	var text string
	contentOK := json.Unmarshal(decoded.Choices[0].Message.Content, &text) == nil && strings.TrimSpace(text) != ""
	reasoning := strings.TrimSpace(decoded.Choices[0].Message.ReasoningContent + decoded.Choices[0].Message.Reasoning)
	// content 为空但返回了思考内容的响应仍是兼容的（思考型模型 token 预算内未作答）。
	if !contentOK && reasoning == "" {
		return providerCheck{Status: "unknown", Code: "incompatible", Message: "服务返回了空内容或不兼容的响应"}
	}
	message := "文本连接测试通过"
	if vision {
		message = "图片输入测试通过"
	}
	return providerCheck{Status: "supported", Message: message}
}

// 只把明确拒绝图像输入的错误标记为不支持；其它无效请求不能推断模型能力。
func rejectedImageInput(data []byte) bool {
	var response struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &response) != nil {
		return false
	}
	code, message := strings.ToLower(response.Error.Code), strings.ToLower(response.Error.Message)
	if code == "unsupported_image" || code == "vision_not_supported" || code == "unsupported_image_input" {
		return true
	}
	image := strings.Contains(message, "image") || strings.Contains(message, "vision") || strings.Contains(message, "multimodal") || strings.Contains(message, "图片")
	unsupported := strings.Contains(message, "not support") || strings.Contains(message, "unsupported") || strings.Contains(message, "不支持")
	return image && unsupported
}
