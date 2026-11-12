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

	"github.com/capyflow/friday/internal/config"
)

// localSetupRequest 保留首次设置里的叫法，判断本身在 localRequest。
func localSetupRequest(r *http.Request) bool { return localRequest(r) }

// localRequest 判断"这是不是本机发起的请求"：远端地址一律不算，浏览器侧还要看
// Host 与 Origin 是否同源。首次设置用它，内置终端也用它（后者是硬闸：非本机不服务）。
func localRequest(r *http.Request) bool {
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
	Protocol    string `json:"protocol"`
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
	protocol, err := config.NormalizeProtocol(req.Protocol)
	if err != nil {
		return ctx.JsonResponse(vhttp.Codes.BadRequest, map[string]string{"error": err.Error()})
	}
	req.Protocol = protocol
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
	probe := probeOpenAI
	switch req.Protocol {
	case config.ProtocolAnthropic:
		probe = probeAnthropic
	case config.ProtocolGemini:
		probe = probeGemini
	}
	result := providerTestResult{Text: probe(ctx, client, req, false), Vision: providerCheck{Status: "not_tested", Message: "未测试图片理解"}}
	result.OK = result.Text.Status == "supported"
	if req.CheckVision && result.OK {
		result.Vision = probe(ctx, client, req, true)
	}
	return result
}

const (
	providerTestImage      = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4//8/AAX+Av4N70a4AAAAAElFTkSuQmCC"
	providerTestImageB64   = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4//8/AAX+Av4N70a4AAAAAElFTkSuQmCC"
	anthropicVersionHeader = "2023-06-01"
	providerTestText       = "Reply with OK. This is a connection test."
	providerTestVisionText = "This is a one-pixel test image. Reply with OK if you can receive it."
)

// providerPost 发一次 JSON 探测请求。失败时返回 nil 与给前端的诊断结果。
func providerPost(ctx context.Context, client *http.Client, endpoint string, body []byte,
	setHeaders func(*http.Request), vision bool) ([]byte, *providerCheck) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &providerCheck{Status: "unknown", Code: "invalid_request", Message: "无法创建模型请求"}
	}
	r.Header.Set("Content-Type", "application/json")
	if setHeaders != nil {
		setHeaders(r)
	}
	response, err := client.Do(r)
	if err != nil {
		code, message := "network", "无法连接模型服务，请检查地址与网络"
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
			code, message = "timeout", "模型服务响应超时，请稍后重试"
		}
		return nil, &providerCheck{Status: "unknown", Code: code, Message: message}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 256*1024+1))
	if err != nil || len(data) > 256*1024 {
		return nil, &providerCheck{Status: "unknown", Code: "response_size", Message: "模型响应过大或无法读取"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		failure := providerHTTPFailure(response.StatusCode, data, vision)
		return nil, &failure
	}
	return data, nil
}

// providerHTTPFailure 把非 2xx 响应翻译成给用户看的诊断（三协议共用）。
func providerHTTPFailure(status int, data []byte, vision bool) providerCheck {
	code, message := "incompatible", "服务响应不兼容，请检查 API 地址、模型名与协议"
	switch {
	case status == 401 || status == 403:
		code, message = "authentication", "鉴权失败，请检查 API Key 和访问权限"
	case status == 404:
		code, message = "not_found", "模型或接口不存在，请检查 API 地址与模型名"
	case status == 429:
		code, message = "rate_limit", "额度不足或请求过于频繁，请稍后重试"
	case status >= 500:
		code, message = "service", "模型服务暂时不可用，请稍后重试"
	}
	statusText := "unknown"
	if vision && (status == 400 || status == 415 || status == 422) && rejectedImageInput(data) {
		statusText, code, message = "unsupported", "vision_unsupported", "模型未接受图片输入，请使用支持图片的模型"
	}
	return providerCheck{Status: statusText, Code: code, Message: message}
}

// probeOpenAI 走 OpenAI 兼容的 /chat/completions。
func probeOpenAI(ctx context.Context, client *http.Client, req providerTestRequest, vision bool) providerCheck {
	var content any = providerTestText
	if vision {
		content = []map[string]any{{"type": "text", "text": providerTestVisionText},
			{"type": "image_url", "image_url": map[string]string{"url": providerTestImage}}}
	}
	// 思考型模型会先消耗 token 思考再作答，预算太小会得到空 content。
	body, _ := json.Marshal(map[string]any{"model": req.Model, "messages": []map[string]any{{"role": "user", "content": content}}, "max_tokens": 512, "stream": false})
	data, failure := providerPost(ctx, client, req.BaseURL+"/chat/completions", body, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+req.APIKey)
	}, vision)
	if failure != nil {
		return *failure
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
	return providerSuccess(vision)
}

// probeAnthropic 走 Anthropic Messages 兼容的 /v1/messages。
func probeAnthropic(ctx context.Context, client *http.Client, req providerTestRequest, vision bool) providerCheck {
	var content any = providerTestText
	if vision {
		content = []map[string]any{
			{"type": "text", "text": providerTestVisionText},
			{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": "image/png", "data": providerTestImageB64,
			}},
		}
	}
	body, _ := json.Marshal(map[string]any{
		"model": req.Model, "max_tokens": 512,
		"messages": []map[string]any{{"role": "user", "content": content}},
	})
	data, failure := providerPost(ctx, client, req.BaseURL+"/v1/messages", body, func(r *http.Request) {
		r.Header.Set("x-api-key", req.APIKey)
		r.Header.Set("anthropic-version", anthropicVersionHeader)
		// Anthropic 官方只认 x-api-key；MiniMax / 智谱等兼容网关按 Bearer 鉴权。
		if !strings.EqualFold(r.URL.Hostname(), "api.anthropic.com") {
			r.Header.Set("Authorization", "Bearer "+req.APIKey)
		}
	}, vision)
	if failure != nil {
		return *failure
	}
	var decoded struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil || len(decoded.Content) == 0 {
		return providerCheck{Status: "unknown", Code: "invalid_response", Message: "服务未返回有效的对话响应，请确认使用 Anthropic 兼容接口"}
	}
	for _, c := range decoded.Content {
		if strings.TrimSpace(c.Text) != "" || strings.TrimSpace(c.Thinking) != "" {
			return providerSuccess(vision)
		}
	}
	return providerCheck{Status: "unknown", Code: "incompatible", Message: "服务返回了空内容或不兼容的响应"}
}

// probeGemini 走 Google Gemini 的 generateContent。
func probeGemini(ctx context.Context, client *http.Client, req providerTestRequest, vision bool) providerCheck {
	parts := []map[string]any{{"text": providerTestText}}
	if vision {
		parts = []map[string]any{{"text": providerTestVisionText},
			{"inlineData": map[string]any{"mimeType": "image/png", "data": providerTestImageB64}}}
	}
	body, _ := json.Marshal(map[string]any{"contents": []map[string]any{{"parts": parts}}})
	endpoint := req.BaseURL + "/v1beta/models/" + url.PathEscape(req.Model) +
		":generateContent?key=" + url.QueryEscape(req.APIKey)
	data, failure := providerPost(ctx, client, endpoint, body, nil, vision)
	if failure != nil {
		return *failure
	}
	var decoded struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return providerCheck{Status: "unknown", Code: "invalid_response", Message: "服务未返回有效的对话响应，请确认使用 Gemini 协议"}
	}
	for _, cand := range decoded.Candidates {
		for _, part := range cand.Content.Parts {
			if strings.TrimSpace(part.Text) != "" {
				return providerSuccess(vision)
			}
		}
	}
	return providerCheck{Status: "unknown", Code: "incompatible", Message: "服务返回了空内容或不兼容的响应"}
}

func providerSuccess(vision bool) providerCheck {
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
