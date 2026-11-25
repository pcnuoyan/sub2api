package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const accountModelTestMaxBytes = 2 << 20
const accountModelTestMaxOutputTokens = 16384

type accountModelTestContextKey struct{}

// accountModelTestOptions 仅随独立后台测试传递，不改变旧连接测试的默认行为。
type accountModelTestOptions struct {
	prompt  string
	effort  string
	secrets []string
}

// accountModelTestOptionsFromContext 读取本次测试的独立参数，普通连接测试返回 nil。
func accountModelTestOptionsFromContext(ctx context.Context) *accountModelTestOptions {
	opts, _ := ctx.Value(accountModelTestContextKey{}).(*accountModelTestOptions)
	return opts
}

// isAccountModelTest 判断当前请求是否使用严格的文本模型测试流程。
func isAccountModelTest(c *gin.Context) bool {
	return c != nil && c.Request != nil && accountModelTestOptionsFromContext(c.Request.Context()) != nil
}

// RunModelTestBackground 执行指定提示词的文本模型测试，保留失败前的答复。
// 任务生命周期由调用方管理；本方法不创建脱离调用方的上下文，不恢复或禁用业务账号。
func (s *AccountTestService) RunModelTestBackground(ctx context.Context, accountID int64, modelID, prompt, reasoningEffort string) (*ScheduledTestResult, error) {
	started := time.Now()
	result := &ScheduledTestResult{Status: "failed", StartedAt: started}
	finish := func(err error) (*ScheduledTestResult, error) {
		result.FinishedAt = time.Now()
		result.LatencyMs = result.FinishedAt.Sub(started).Milliseconds()
		if err != nil {
			result.ErrorMessage = err.Error()
		}
		return result, nil
	}
	if ctx == nil {
		return finish(errors.New("测试上下文不能为空"))
	}
	if err := ctx.Err(); err != nil {
		return finish(err)
	}
	if strings.TrimSpace(modelID) == "" || strings.TrimSpace(prompt) == "" {
		return finish(errors.New("模型和提示词不能为空"))
	}
	effort := strings.TrimSpace(reasoningEffort)
	switch effort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		return finish(fmt.Errorf("不支持的 reasoning_effort：%s", effort))
	}
	if len(prompt) > accountModelTestMaxBytes {
		return finish(errors.New("测试提示词超过 2 MiB 限制"))
	}
	testCtx, cancel := context.WithCancel(context.WithValue(ctx, accountModelTestContextKey{}, &accountModelTestOptions{prompt: prompt, effort: effort}))
	defer cancel()
	w := &accountModelTestRecorder{header: make(http.Header), cancel: cancel}
	c, _ := gin.CreateTestContext(w)
	c.Request = (&http.Request{}).WithContext(testCtx)
	c.Set(accountTestBackgroundKey, true)
	testErr := s.TestAccountConnection(c, accountID, modelID, prompt, AccountTestModeDefault)
	body, overflow := w.snapshot()
	result.ResponseText, result.ErrorMessage = parseTestSSEOutput(body)
	defer func() { result.ErrorMessage = redactAccountModelTestError(testCtx, result.ErrorMessage) }()
	if overflow {
		return finish(errors.New("测试输出超过 2 MiB 限制"))
	}
	if err := ctx.Err(); err != nil {
		return finish(err)
	}
	if testErr != nil {
		return finish(testErr)
	}
	if result.ErrorMessage != "" {
		return finish(nil)
	}
	complete := false
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			var event TestEvent
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event.Type == "test_complete" && event.Success {
				complete = true
			}
		}
	}
	if !complete || strings.TrimSpace(result.ResponseText) == "" {
		return finish(errors.New("上游未明确完成或没有返回文本回答"))
	}
	result.Status = "success"
	return finish(nil)
}

// accountModelTestRecorder 为后台 SSE 提供有界、并发安全的内存写入器。
type accountModelTestRecorder struct {
	mu       sync.Mutex
	header   http.Header
	body     bytes.Buffer
	overflow bool
	cancel   context.CancelFunc
}

// Header 返回响应头；测试流程在输出事件前完成头部设置。
func (w *accountModelTestRecorder) Header() http.Header { return w.header }

// WriteHeader 满足内存响应接口，业务结果由 SSE 事件决定。
func (w *accountModelTestRecorder) WriteHeader(int) {}

// Flush 满足 SSE 刷新接口，内存输出无需额外刷新。
func (w *accountModelTestRecorder) Flush() {}

// Write 仅保留完整写入片段，超限时取消上游以防继续累计内存。
func (w *accountModelTestRecorder) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.overflow || w.body.Len()+len(p) > accountModelTestMaxBytes {
		w.overflow = true
		w.cancel()
		return 0, errors.New("测试输出超过 2 MiB 限制")
	}
	return w.body.Write(p)
}

// snapshot 返回完成后的不可变输出与超限状态。
func (w *accountModelTestRecorder) snapshot() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String(), w.overflow
}

// validateAccountModelTestTarget 在凭据处理和媒体路由之前拦截非文本测试目标。
func (s *AccountTestService) validateAccountModelTestTarget(c *gin.Context, account *Account, model string) error {
	if !isAccountModelTest(c) {
		return nil
	}
	rememberAccountModelTestSecrets(c.Request.Context(), account)
	if account.IsTypeSafe() || strings.EqualFold(account.Platform, "sora") || account.IsSyntheticUITest() {
		return errors.New("该账号类型不支持文本模型回答测试")
	}
	if account.ProxyID != nil && account.Proxy == nil {
		return errors.New("账号已配置的代理不存在，拒绝改为直连")
	}
	if capabilities, configured := account.openAIEndpointCapabilitySet(); configured && account.IsOpenAI() && !capabilities[string(OpenAIEndpointCapabilityChatCompletions)] && !capabilities[string(OpenAIEndpointCapabilityResponses)] {
		return errors.New("该账号未启用文本生成端点")
	}
	if err := validateAccountModelTestModel(model); err != nil {
		return err
	}
	if err := validateAccountModelTestModel(account.GetMappedModel(model)); err != nil {
		return err
	}
	if account.Platform == PlatformAntigravity && account.Type != AccountTypeAPIKey && s.antigravityGatewayService != nil {
		return validateAccountModelTestModel(s.antigravityGatewayService.getMappedModel(account, model))
	}
	return nil
}

var accountModelTestURLCredentials = regexp.MustCompile(`(?i)(https?://|socks5h?://)[^\s/@]+@`)
var accountModelTestBearer = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]+=*`)

// rememberAccountModelTestSecrets 仅在当前任务内记住需要从错误文本移除的凭据。
func rememberAccountModelTestSecrets(ctx context.Context, account *Account) {
	opts := accountModelTestOptionsFromContext(ctx)
	if opts == nil || account == nil {
		return
	}
	for _, key := range []string{"api_key", "access_token", "refresh_token", "client_secret", "private_key", "password", "id_token", "token", "secret_access_key", "session_token"} {
		if value := account.GetCredential(key); value != "" {
			opts.secrets = append(opts.secrets, value)
		}
	}
	if account.Proxy != nil && account.Proxy.Password != "" {
		opts.secrets = append(opts.secrets, account.Proxy.Password)
	}
}

// redactAccountModelTestError 在写日志、持久化或导出前移除已知凭据和 URL 中的鉴权部分。
func redactAccountModelTestError(ctx context.Context, message string) string {
	if opts := accountModelTestOptionsFromContext(ctx); opts != nil {
		for _, secret := range opts.secrets {
			message = strings.ReplaceAll(message, secret, "***")
		}
	}
	message = accountModelTestURLCredentials.ReplaceAllString(message, `${1}***@`)
	message = accountModelTestBearer.ReplaceAllString(message, "Bearer ***")
	return truncateString(logredact.RedactText(sanitizeUpstreamErrorMessage(message), "api_key", "authorization", "token", "key"), 4096)
}

// validateAccountModelTestModel 同时用于公开模型名和最终映射名，防止别名绕过媒体限制。
func validateAccountModelTestModel(model string) error {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return errors.New("模型映射结果为空")
	}
	for _, kind := range []string{"image", "imagine", "imagen", "dall-e", "flux", "stable-diffusion", "sdxl", "seedream", "seedance", "video", "sora", "veo", "audio", "voice", "tts", "stt", "whisper", "embed", "rerank", "transcri", "speech", "music", "system-one", "systemone"} {
		if strings.Contains(name, kind) {
			return fmt.Errorf("模型 %s 不是受支持的文本生成模型", model)
		}
	}
	return nil
}

// applyAccountModelTestPayload 仅覆盖新测试的用户输入及明确支持的原生推理字段。
func applyAccountModelTestPayload(c *gin.Context, payload map[string]any, protocol, model string) error {
	if !isAccountModelTest(c) {
		return nil
	}
	opts := accountModelTestOptionsFromContext(c.Request.Context())
	if err := validateAccountModelTestModel(model); err != nil {
		return err
	}
	switch protocol {
	case APIProtocolResponses:
		payload["input"] = []map[string]any{{"role": "user", "content": []map[string]any{{"type": "input_text", "text": opts.prompt}}}}
		if _, capped := payload["max_output_tokens"]; capped {
			payload["max_output_tokens"] = accountModelTestMaxOutputTokens
		}
		if opts.effort != "" {
			payload["reasoning"] = map[string]any{"effort": opts.effort}
		}
	case APIProtocolChatCompletions:
		payload["messages"] = []map[string]any{{"role": "user", "content": opts.prompt}}
		for _, field := range []string{"max_tokens", "max_completion_tokens"} {
			if _, capped := payload[field]; capped {
				payload[field] = accountModelTestMaxOutputTokens
			}
		}
		if opts.effort != "" {
			payload["reasoning_effort"] = opts.effort
		}
	case APIProtocolAnthropic:
		payload["messages"] = []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": opts.prompt}}}}
		payload["max_tokens"] = accountModelTestMaxOutputTokens
		if opts.effort != "" {
			supported := false
			for _, level := range claude.EffortLevelsForModel(model) {
				supported = supported || level == opts.effort
			}
			if !supported {
				return fmt.Errorf("模型 %s 的 Anthropic 协议不支持 reasoning_effort=%s", model, opts.effort)
			}
			payload["output_config"] = map[string]any{"effort": opts.effort}
		}
	case "gemini":
		payload["contents"] = []map[string]any{{"role": "user", "parts": []map[string]any{{"text": opts.prompt}}}}
		generation, _ := payload["generationConfig"].(map[string]any)
		if generation == nil {
			generation = make(map[string]any)
			payload["generationConfig"] = generation
		}
		generation["maxOutputTokens"] = accountModelTestMaxOutputTokens
		if opts.effort != "" {
			// 只使用模型原生档位，不把 none/xhigh/max 或预算数值猜测转换成其他档位。
			if geminiThinkingParamForModel(model) != geminiThinkingParamLevel || geminiModelNameCarriesThinkingTier(model) ||
				(opts.effort != "minimal" && opts.effort != "low" && opts.effort != "medium" && opts.effort != "high") ||
				geminiThinkingLevelFor(model, claudeThinkingIntent{tier: opts.effort}) != opts.effort {
				return fmt.Errorf("模型 %s 的 Gemini 协议不支持原样传递 reasoning_effort=%s", model, opts.effort)
			}
			generation["thinkingConfig"] = map[string]any{"thinkingLevel": opts.effort}
		}
	default:
		return fmt.Errorf("不支持的模型测试协议：%s", protocol)
	}
	return nil
}

// applyAccountModelTestJSON 在已有 JSON 构造器后应用测试参数，保留旧流程的请求构造逻辑。
func applyAccountModelTestJSON(c *gin.Context, body []byte, protocol, model string) ([]byte, error) {
	if !isAccountModelTest(c) {
		return body, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if err := applyAccountModelTestPayload(c, payload, protocol, model); err != nil {
		return nil, err
	}
	return json.Marshal(payload)
}

// readAccountTestBody 限制新测试的非流式响应及错误体大小，旧连接测试保持原读取行为。
func readAccountTestBody(ctx context.Context, body io.Reader) ([]byte, error) {
	if accountModelTestOptionsFromContext(ctx) == nil {
		return io.ReadAll(body)
	}
	if closer, ok := body.(io.Closer); ok {
		stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
		defer stop()
	}
	b, err := io.ReadAll(io.LimitReader(body, accountModelTestMaxBytes+1))
	if len(b) > accountModelTestMaxBytes {
		return b[:accountModelTestMaxBytes], errors.New("上游响应超过 2 MiB 限制")
	}
	return b, err
}

// accountModelTestStreamState 只记录当前响应的可见文本和协议终态。
type accountModelTestStreamState struct {
	protocol   string
	hasText    bool
	stopReason string
	done       bool
}

// processAccountModelTestJSONResult 消费完整非流式 JSON，避免格式化换行干扰协议判断。
func (s *AccountTestService) processAccountModelTestJSONResult(c *gin.Context, body []byte, protocol string) error {
	if err := c.Request.Context().Err(); err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	if len(body) > accountModelTestMaxBytes || !gjson.ValidBytes(body) {
		return s.sendErrorAndEnd(c, "上游 JSON 无效或超过 2 MiB 限制")
	}
	state := &accountModelTestStreamState{protocol: protocol}
	texts, err := state.consume(gjson.ParseBytes(body))
	for _, text := range texts {
		s.sendEvent(c, TestEvent{Type: "content", Text: text})
	}
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	if !state.done || !state.hasText {
		return s.sendErrorAndEnd(c, "上游未明确完成或没有返回文本回答")
	}
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

// processAccountModelTestStream 有界读取 SSE，只有成功终态且存在文本时发出完成事件。
func (s *AccountTestService) processAccountModelTestStream(c *gin.Context, body io.Reader, protocol string) error {
	ctx := c.Request.Context()
	if closer, ok := body.(io.Closer); ok {
		stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
		defer stop()
	}
	limited := &io.LimitedReader{R: body, N: accountModelTestMaxBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), accountModelTestMaxBytes+1)
	state := &accountModelTestStreamState{protocol: protocol}
	var pending strings.Builder
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return s.sendErrorAndEnd(c, err.Error())
		}
		if limited.N <= 0 {
			return s.sendErrorAndEnd(c, "上游 SSE 超过 2 MiB 限制")
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			if pending.Len() > 0 {
				return s.sendErrorAndEnd(c, "上游返回无效 JSON")
			}
			continue
		}
		if strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		} else if !strings.HasPrefix(line, "{") && pending.Len() == 0 {
			continue
		}
		if line == "[DONE]" {
			if !state.done {
				return s.sendErrorAndEnd(c, "上游结束标记前未返回成功终态")
			}
			break
		}
		if pending.Len() > 0 {
			pending.WriteByte('\n')
		}
		pending.WriteString(line)
		if !gjson.Valid(pending.String()) {
			continue
		}
		texts, err := state.consume(gjson.Parse(pending.String()))
		pending.Reset()
		for _, text := range texts {
			s.sendEvent(c, TestEvent{Type: "content", Text: text})
		}
		if err != nil {
			return s.sendErrorAndEnd(c, err.Error())
		}
		if state.done {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	if err := scanner.Err(); err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("读取模型响应失败：%s", err))
	}
	if !state.done {
		return s.sendErrorAndEnd(c, "上游流在成功终态之前结束")
	}
	if !state.hasText {
		return s.sendErrorAndEnd(c, "上游已结束，但未返回文本回答")
	}
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

// consume 按原生协议识别回答与终态；思考摘要、工具调用和截断回答不计为成功。
func (state *accountModelTestStreamState) consume(data gjson.Result) ([]string, error) {
	var texts []string
	add := func(text string) {
		if text != "" {
			texts = append(texts, text)
			state.hasText = state.hasText || strings.TrimSpace(text) != ""
		}
	}
	addResponseOutput := func(response gjson.Result) {
		if state.hasText {
			return
		}
		for _, item := range response.Get("output").Array() {
			for _, part := range item.Get("content").Array() {
				if part.Get("type").String() == "output_text" {
					add(part.Get("text").String())
				}
			}
		}
	}
	if data.Get("error").Exists() {
		return texts, fmt.Errorf("上游返回错误：%s", data.Get("error.message").String())
	}
	switch state.protocol {
	case APIProtocolResponses:
		event := data.Get("type").String()
		switch event {
		case "response.output_text.delta":
			add(data.Get("delta").String())
		case "response.failed", "response.incomplete", "error":
			addResponseOutput(data.Get("response"))
			return texts, fmt.Errorf("上游返回 %s：%s", event, data.Get("response.error.message").String())
		case "response.completed", "response.done", "response":
			response := data.Get("response")
			if !response.Exists() {
				response = data
			}
			status := response.Get("status").String()
			if (status != "" && status != "completed") || (event != "response.completed" && status != "completed") {
				return texts, fmt.Errorf("上游响应不是成功终态：%s", status)
			}
			addResponseOutput(response)
			state.done = true
		default:
			if data.Get("object").String() == "response" {
				body, _ := json.Marshal(map[string]any{"type": "response", "response": json.RawMessage(data.Raw)})
				return state.consume(gjson.ParseBytes(body))
			}
		}
	case APIProtocolChatCompletions:
		choices := data.Get("choices").Array()
		if len(choices) > 0 {
			choice := choices[0]
			add(choice.Get("delta.content").String())
			add(choice.Get("message.content").String())
			if reason := choice.Get("finish_reason").String(); reason != "" {
				if reason != "stop" {
					return texts, fmt.Errorf("Chat Completions 未正常完成：%s", reason)
				}
				state.done = true
			}
		}
	case APIProtocolAnthropic:
		switch data.Get("type").String() {
		case "content_block_delta":
			if data.Get("delta.type").String() != "thinking_delta" {
				add(data.Get("delta.text").String())
			}
		case "message_delta":
			state.stopReason = data.Get("delta.stop_reason").String()
		case "message_stop":
			if state.stopReason != "end_turn" && state.stopReason != "stop_sequence" {
				return texts, fmt.Errorf("Anthropic 未正常完成：%s", state.stopReason)
			}
			state.done = true
		case "message":
			for _, part := range data.Get("content").Array() {
				if part.Get("type").String() == "text" {
					add(part.Get("text").String())
				}
			}
			reason := data.Get("stop_reason").String()
			if reason != "end_turn" && reason != "stop_sequence" {
				return texts, fmt.Errorf("Anthropic 未正常完成：%s", reason)
			}
			state.done = true
		case "error":
			return texts, errors.New("Anthropic 返回错误事件")
		}
	case "gemini":
		if response := data.Get("response"); response.Exists() {
			data = response
		}
		if data.Get("error").Exists() {
			return texts, fmt.Errorf("Gemini 返回错误：%s", data.Get("error.message").String())
		}
		if block := data.Get("promptFeedback.blockReason").String(); block != "" {
			return texts, fmt.Errorf("Gemini 拒绝请求：%s", block)
		}
		candidates := data.Get("candidates").Array()
		if len(candidates) > 0 {
			candidate := candidates[0]
			for _, part := range candidate.Get("content.parts").Array() {
				if !part.Get("thought").Bool() {
					add(part.Get("text").String())
				}
			}
			if reason := candidate.Get("finishReason").String(); reason != "" {
				if reason != "STOP" {
					return texts, fmt.Errorf("Gemini 未正常完成：%s", reason)
				}
				state.done = true
			}
		}
	}
	return texts, nil
}

// testAntigravityModelConnection 复用协议构造，独立发送一次以避免业务重试的账号状态副作用。
func (s *AccountTestService) testAntigravityModelConnection(c *gin.Context, account *Account, model string) error {
	gateway := s.antigravityGatewayService
	if gateway == nil || gateway.httpUpstream == nil {
		return s.sendErrorAndEnd(c, "Antigravity gateway not configured")
	}
	ctx := c.Request.Context()
	opts := accountModelTestOptionsFromContext(ctx)
	mapped := gateway.getMappedModel(account, model)
	if err := validateAccountModelTestModel(mapped); err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	// Claude 经 Antigravity 转换后不保留原生 effort 档位，因此拒绝显式值，避免暗中映射。
	if opts.effort != "" && !shouldUseAntigravityGeminiTestRequest(model, mapped) {
		return s.sendErrorAndEnd(c, "Antigravity Claude 路径不支持原样传递 reasoning_effort")
	}
	project, err := resolveAntigravityProjectID(account)
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	var body []byte
	if shouldUseAntigravityGeminiTestRequest(model, mapped) {
		body, err = gateway.buildGeminiTestRequestWithPrompt(project, mapped, opts.prompt)
	} else {
		body, err = gateway.buildClaudeTestRequest(project, mapped, opts.prompt)
	}
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	inner, ok := envelope["request"].(map[string]any)
	if !ok {
		return s.sendErrorAndEnd(c, "Antigravity 请求缺少 request")
	}
	if shouldUseAntigravityGeminiTestRequest(model, mapped) {
		if err := applyAccountModelTestPayload(c, inner, "gemini", mapped); err != nil {
			return s.sendErrorAndEnd(c, err.Error())
		}
	} else {
		generation, _ := inner["generationConfig"].(map[string]any)
		if generation == nil {
			generation = make(map[string]any)
			inner["generationConfig"] = generation
		}
		generation["maxOutputTokens"] = accountModelTestMaxOutputTokens
	}
	body, err = json.Marshal(envelope)
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	token, err := accountModelTestAccessToken(account)
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	baseURL := resolveAntigravityForwardBaseURL(account)
	if baseURL == "" {
		return s.sendErrorAndEnd(c, "Antigravity base URL not configured")
	}
	request, err := antigravity.NewAPIRequestWithURL(ctx, baseURL, "streamGenerateContent", token, body)
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	response, err := gateway.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	if response == nil {
		return s.sendErrorAndEnd(c, "Antigravity 上游返回空响应")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Antigravity 返回 HTTP %d", response.StatusCode))
	}
	return s.processAccountModelTestStream(c, response.Body, "gemini")
}

// accountModelTestAccessToken 使用现有凭据进行只读测试，过期时明确提示刷新而不修改账号状态。
func accountModelTestAccessToken(account *Account) (string, error) {
	if account.ProxyID != nil && account.Proxy == nil {
		return "", errors.New("账号已配置的代理不存在，拒绝改为直连")
	}
	if account.Type == AccountTypeUpstream {
		if token := strings.TrimSpace(account.GetCredential("api_key")); token != "" {
			return token, nil
		}
		return "", errors.New("账号缺少 API key")
	}
	if expires := account.GetCredentialAsTime("expires_at"); expires != nil && !time.Now().Before(*expires) {
		return "", errors.New("账号访问令牌已过期，请刷新令牌后重试")
	}
	token := strings.TrimSpace(account.GetCredential("access_token"))
	if account.Platform == PlatformGrok {
		token = strings.TrimSpace(account.GetGrokAccessToken())
	}
	if token == "" {
		return "", errors.New("账号缺少访问令牌")
	}
	return token, nil
}
