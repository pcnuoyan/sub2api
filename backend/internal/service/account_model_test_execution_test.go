//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const modelTestResponsesOK = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
const modelTestChatOK = "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n"
const modelTestClaudeOK = "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
const modelTestGeminiOK = "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"answer\"}]},\"finishReason\":\"STOP\"}]}\n\n"

// modelTestFixture 只提供内存上游，防止测试意外访问真实模型服务。
func modelTestFixture(account *Account, stream string) (*AccountTestService, *httpUpstreamRecorder, *openAIAccountTestRepo) {
	repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}}
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(stream))}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream, cfg: rawChatCompletionsTestConfig()}
	svc.antigravityGatewayService = &AntigravityGatewayService{httpUpstream: upstream}
	return svc, upstream, repo
}

// modelTestAccount 为不同协议构造互相隔离的测试账号。
func modelTestAccount(platform string) *Account {
	return &Account{ID: 900, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Concurrency: 1,
		Credentials: map[string]any{"api_key": "local-test-key", "base_url": "https://upstream.invalid"},
		Extra:       map[string]any{openai_compat.ExtraKeyResponsesSupported: true}}
}

// TestAccountModelTestPromptAndEffort 验证各协议真实请求体携带用户提示词及原生档位。
func TestAccountModelTestPromptAndEffort(t *testing.T) {
	for _, tc := range []struct {
		name, platform, model, stream, promptPath, effortPath string
		oauth, chat, adaptive                                 bool
	}{
		{"responses", PlatformOpenAI, "gpt-5.4", modelTestResponsesOK, "input.0.content.0.text", "reasoning.effort", false, false, false},
		{"oauth", PlatformOpenAI, "gpt-5.4", modelTestResponsesOK, "input.0.content.0.text", "reasoning.effort", true, false, false},
		{"chat", PlatformOpenAI, "gpt-5.4", modelTestChatOK, "messages.0.content", "reasoning_effort", false, true, false},
		{"claude", PlatformAnthropic, "claude-sonnet-4-6", modelTestClaudeOK, "messages.0.content.0.text", "output_config.effort", false, false, false},
		{"gemini", PlatformGemini, "gemini-3.1-pro", modelTestGeminiOK, "contents.0.parts.0.text", "generationConfig.thinkingConfig.thinkingLevel", false, false, false},
		{"grok", PlatformGrok, "grok-4.3", modelTestResponsesOK, "input.0.content.0.text", "reasoning.effort", false, false, false},
		{"adaptive", PlatformDeepseek, "deepseek-v4-flash", modelTestChatOK, "messages.0.content", "reasoning_effort", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := modelTestAccount(tc.platform)
			if tc.oauth {
				account.Type = AccountTypeOAuth
				account.Credentials["access_token"] = "local-oauth-token"
			}
			if tc.chat {
				account.Extra[openai_compat.ExtraKeyResponsesSupported] = false
			}
			if tc.adaptive {
				account.Credentials["api_protocol"] = APIProtocolAdaptive
			}
			svc, upstream, _ := modelTestFixture(account, tc.stream)
			result, err := svc.RunModelTestBackground(context.Background(), account.ID, tc.model, "请绘制题目要求的 SVG，保留空格。", "high")
			require.NoError(t, err)
			require.Equal(t, "success", result.Status, result.ErrorMessage)
			require.Equal(t, "answer", result.ResponseText)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, "请绘制题目要求的 SVG，保留空格。", gjson.GetBytes(upstream.lastBody, tc.promptPath).String())
			require.Equal(t, "high", gjson.GetBytes(upstream.lastBody, tc.effortPath).String())
		})
	}
}

// TestAccountModelTestTerminalAndPartialAnswer 覆盖完成、截断、空答复及协议拒绝的区别。
func TestAccountModelTestTerminalAndPartialAnswer(t *testing.T) {
	for _, tc := range []struct {
		name, platform, model, stream, wantText string
		success                                 bool
	}{
		{"responses-eof", PlatformOpenAI, "gpt-5.4", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", "partial", false},
		{"responses-empty", PlatformOpenAI, "gpt-5.4", "data: {\"type\":\"response.completed\"}\n\n", "", false},
		{"responses-incomplete", PlatformOpenAI, "gpt-5.4", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\ndata: {\"type\":\"response.incomplete\"}\n\n", "partial", false},
		{"responses-done-failed", PlatformOpenAI, "gpt-5.4", "data: {\"type\":\"response.done\",\"response\":{\"status\":\"failed\"}}\n\n", "", false},
		{"responses-final-only", PlatformOpenAI, "gpt-5.4", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"content\":[{\"type\":\"output_text\",\"text\":\"final\"}]}]}}", "final", true},
		{"claude-eof", PlatformAnthropic, "claude-sonnet-4-6", "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"partial\"}}\n\n", "partial", false},
		{"claude-limit", PlatformAnthropic, "claude-sonnet-4-6", strings.ReplaceAll(modelTestClaudeOK, "end_turn", "max_tokens"), "answer", false},
		{"gemini-eof", PlatformGemini, "gemini-3.1-pro", strings.ReplaceAll(modelTestGeminiOK, `,"finishReason":"STOP"`, ""), "answer", false},
		{"gemini-safety", PlatformGemini, "gemini-3.1-pro", strings.ReplaceAll(modelTestGeminiOK, "STOP", "SAFETY"), "answer", false},
		{"gemini-thought-only", PlatformGemini, "gemini-3.1-pro", strings.ReplaceAll(modelTestGeminiOK, `"text":"answer"`, `"text":"reasoning","thought":true`), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _ := modelTestFixture(modelTestAccount(tc.platform), tc.stream)
			result, err := svc.RunModelTestBackground(context.Background(), 900, tc.model, "prompt", "")
			require.NoError(t, err)
			require.Equal(t, tc.success, result.Status == "success", result.ErrorMessage)
			require.Equal(t, tc.wantText, result.ResponseText)
			if !tc.success {
				require.NotEmpty(t, result.ErrorMessage)
			}
		})
	}
}

// TestAccountModelTestChatRequiresSuccessfulFinish 防止仅 DONE 或 length 截断被当成成功。
func TestAccountModelTestChatRequiresSuccessfulFinish(t *testing.T) {
	for _, stream := range []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\ndata: [DONE]\n\n",
		strings.ReplaceAll(modelTestChatOK, "stop", "length"),
	} {
		account := modelTestAccount(PlatformOpenAI)
		account.Extra[openai_compat.ExtraKeyResponsesSupported] = false
		svc, _, _ := modelTestFixture(account, stream)
		result, err := svc.RunModelTestBackground(context.Background(), 900, "gpt-5.4", "prompt", "")
		require.NoError(t, err)
		require.Equal(t, "failed", result.Status)
		require.NotEmpty(t, result.ResponseText)
	}
}

// TestAccountModelTestRejectsUnsupportedEffort 不允许把用户选择的档位默默降级。
func TestAccountModelTestRejectsUnsupportedEffort(t *testing.T) {
	for _, tc := range []struct{ platform, model, effort string }{
		{PlatformAnthropic, "claude-sonnet-4-6", "none"},
		{PlatformGemini, "gemini-2.5-flash", "high"},
		{PlatformGemini, "gemini-3.1-pro", "xhigh"},
		{PlatformGemini, "gemini-3.1-pro", "minimal"},
		{PlatformOpenAI, "gpt-5.4", "invented"},
	} {
		t.Run(tc.model+tc.effort, func(t *testing.T) {
			svc, upstream, _ := modelTestFixture(modelTestAccount(tc.platform), "")
			result, err := svc.RunModelTestBackground(context.Background(), 900, tc.model, "prompt", tc.effort)
			require.NoError(t, err)
			require.Equal(t, "failed", result.Status)
			require.Empty(t, upstream.requests)
			require.Contains(t, result.ErrorMessage, "reasoning_effort")
		})
	}
}

// TestAccountModelTestBlocksMediaAndMappedAliases 校验映射前后均不能触发媒体生成或非文本平台。
func TestAccountModelTestBlocksMediaAndMappedAliases(t *testing.T) {
	for _, model := range []string{"gpt-image-2", "sora-2", "grok-imagine-video", "tts-1", "text-embedding-3-large", "whisper-1", "gemini-2.5-flash-audio"} {
		for _, mapped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", model, mapped), func(t *testing.T) {
				account := modelTestAccount(PlatformOpenAI)
				requested := model
				if mapped {
					requested = "text-alias"
					account.Credentials["model_mapping"] = map[string]any{requested: model}
				}
				svc, upstream, _ := modelTestFixture(account, "")
				result, err := svc.RunModelTestBackground(context.Background(), 900, requested, "prompt", "")
				require.NoError(t, err)
				require.Equal(t, "failed", result.Status)
				require.Empty(t, upstream.requests)
			})
		}
	}
	for _, platform := range []string{PlatformTypeSafe, "sora"} {
		svc, upstream, _ := modelTestFixture(modelTestAccount(platform), "")
		result, err := svc.RunModelTestBackground(context.Background(), 900, "arbitrary", "prompt", "")
		require.NoError(t, err)
		require.Equal(t, "failed", result.Status)
		require.Empty(t, upstream.requests)
	}
}

// TestAccountModelTestAntigravityPrompt 使用内存上游验证中转协议及严格终态，无真实请求。
func TestAccountModelTestAntigravityPrompt(t *testing.T) {
	for _, model := range []string{"gemini-3.1-pro", "claude-sonnet-4-6"} {
		t.Run(model, func(t *testing.T) {
			account := modelTestAccount(PlatformAntigravity)
			account.Type = AccountTypeUpstream
			account.Credentials["project_id"] = "local-project"
			account.Credentials["model_mapping"] = map[string]any{model: model}
			svc, upstream, _ := modelTestFixture(account, modelTestGeminiOK)
			result, err := svc.RunModelTestBackground(context.Background(), 900, model, "unique-user-prompt", "")
			require.NoError(t, err)
			require.Equal(t, "success", result.Status, result.ErrorMessage)
			require.Contains(t, string(upstream.lastBody), "unique-user-prompt")
			require.Equal(t, int64(accountModelTestMaxOutputTokens), gjson.GetBytes(upstream.lastBody, "request.generationConfig.maxOutputTokens").Int())
		})
	}
}

// TestAccountModelTestCancellationClosesStream 验证取消会终止阻塞读取且保留此前内容。
func TestAccountModelTestCancellationClosesStream(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	svc, upstream, _ := modelTestFixture(modelTestAccount(PlatformOpenAI), "")
	upstream.resp.Body = reader
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan *ScheduledTestResult, 1)
	go func() { result, _ := svc.RunModelTestBackground(ctx, 900, "gpt-5.4", "prompt", ""); resultCh <- result }()
	_, err := io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
	require.NoError(t, err)
	// 等待下一次读取开始，确认首段已经被解析，而不是仅被 bufio 预读。
	_, err = io.WriteString(writer, ": heartbeat\n\n")
	require.NoError(t, err)
	cancel()
	select {
	case result := <-resultCh:
		require.Equal(t, "failed", result.Status)
		require.Contains(t, result.ErrorMessage, "canceled")
		require.Equal(t, "partial", result.ResponseText)
	case <-time.After(2 * time.Second):
		t.Fatal("取消后测试仍然阻塞")
	}
}

// TestAccountModelTestOutputBound 验证上游和输出记录器均有硬性内存上限。
func TestAccountModelTestOutputBound(t *testing.T) {
	stream := strings.Repeat(": padding\n", accountModelTestMaxBytes/10+1)
	svc, _, _ := modelTestFixture(modelTestAccount(PlatformOpenAI), stream)
	result, err := svc.RunModelTestBackground(context.Background(), 900, "gpt-5.4", "prompt", "")
	require.NoError(t, err)
	require.Equal(t, "failed", result.Status)
	require.Contains(t, result.ErrorMessage, "2 MiB")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder := &accountModelTestRecorder{header: make(http.Header), cancel: cancel}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = recorder.Write([]byte(strings.Repeat("x", accountModelTestMaxBytes/4)))
		}()
	}
	wg.Wait()
	output, overflow := recorder.snapshot()
	require.True(t, overflow)
	require.LessOrEqual(t, len(output), accountModelTestMaxBytes)
	require.Error(t, ctx.Err())
}

// TestAccountModelTestDoesNotChangeAccountState 隔离旧连通测试对 401、429 的状态同步。
func TestAccountModelTestDoesNotChangeAccountState(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		svc, upstream, repo := modelTestFixture(modelTestAccount(PlatformOpenAI), `{"error":{"message":"test failure"}}`)
		upstream.resp.StatusCode = status
		upstream.resp.Header.Set("x-ratelimit-reset-requests", "60s")
		result, err := svc.RunModelTestBackground(context.Background(), 900, "gpt-5.4", "prompt", "")
		require.NoError(t, err)
		require.Equal(t, "failed", result.Status)
		require.Zero(t, repo.setErrorID)
		require.Zero(t, repo.rateLimitedID)
		require.Zero(t, repo.clearedErrorID)
		require.Empty(t, repo.updatedExtra)
	}
}

// TestAccountModelTestLegacyDefaultsRemainUnchanged 确认旧入口仍使用原探针提示词与成功标准。
func TestAccountModelTestLegacyDefaultsRemainUnchanged(t *testing.T) {
	svc, upstream, _ := modelTestFixture(modelTestAccount(PlatformOpenAI), "data: {\"type\":\"response.completed\"}\n\n")
	result, err := svc.RunTestBackground(context.Background(), 900, "gpt-5.4")
	require.NoError(t, err)
	require.Equal(t, "success", result.Status)
	require.Equal(t, "hi", gjson.GetBytes(upstream.lastBody, "input.0.content.0.text").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "reasoning").Exists())
}

// TestAccountModelTestMultilineSSE 验证一个 SSE 事件可使用多行 data 承载 JSON。
func TestAccountModelTestMultilineSSE(t *testing.T) {
	stream := "data: {\n" + "data: \"type\":\"response.output_text.delta\",\n" + "data: \"delta\":\"answer\"}\n\n" + "data: {\"type\":\"response.completed\"}\n\n"
	svc, _, _ := modelTestFixture(modelTestAccount(PlatformOpenAI), stream)
	result, err := svc.RunModelTestBackground(context.Background(), 900, "gpt-5.4", "prompt", "")
	require.NoError(t, err)
	require.Equal(t, "success", result.Status, result.ErrorMessage)
	require.Equal(t, "answer", result.ResponseText)
}

// TestAccountModelTestErrorRedaction 确认持久化错误不包含凭据实值、代理用户信息或 Bearer。
func TestAccountModelTestErrorRedaction(t *testing.T) {
	svc, upstream, _ := modelTestFixture(modelTestAccount(PlatformOpenAI), "")
	upstream.err = fmt.Errorf("request to http://proxy-user:proxy-secret@example.invalid failed: access_token=secret-token Authorization: Bearer bearer-token key=secret-key local-test-key")
	result, err := svc.RunModelTestBackground(context.Background(), 900, "gpt-5.4", "prompt", "")
	require.NoError(t, err)
	require.Equal(t, "failed", result.Status)
	for _, secret := range []string{"proxy-user", "proxy-secret", "secret-token", "bearer-token", "secret-key", "local-test-key"} {
		require.NotContains(t, result.ErrorMessage, secret)
	}
}

// TestAccountModelTestBedrockPrettyJSON 验证非流式格式化 JSON 与超限响应不会混淆。
func TestAccountModelTestBedrockPrettyJSON(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		t.Run(fmt.Sprint(overflow), func(t *testing.T) {
			account := modelTestAccount(PlatformAnthropic)
			account.Type = AccountTypeBedrock
			account.Credentials["auth_mode"] = "apikey"
			account.Credentials["region"] = "us-east-1"
			body := "{\n\n  \"type\": \"message\",\n  \"content\": [{\"type\":\"text\",\"text\":\"answer\"}],\n  \"stop_reason\":\"end_turn\"\n}"
			if overflow {
				body += strings.Repeat(" ", accountModelTestMaxBytes)
			}
			svc, upstream, _ := modelTestFixture(account, body)
			result, err := svc.RunModelTestBackground(context.Background(), 900, "claude-sonnet-4-6", "bedrock-user-prompt", "")
			require.NoError(t, err)
			if overflow {
				require.Equal(t, "failed", result.Status)
				require.Contains(t, result.ErrorMessage, "2 MiB")
			} else {
				require.Equal(t, "success", result.Status, result.ErrorMessage)
				require.Equal(t, "answer", result.ResponseText)
				require.Equal(t, "bedrock-user-prompt", gjson.GetBytes(upstream.lastBody, "messages.0.content.0.text").String())
			}
		})
	}
}

// TestAccountModelTestProviderNativeProtocols 覆盖供应商原生端点与按模型路由的提示词传递。
func TestAccountModelTestProviderNativeProtocols(t *testing.T) {
	for _, tc := range []struct{ platform, protocol, model, stream, promptPath string }{
		{PlatformDeepseek, APIProtocolResponses, "deepseek-v4-flash", modelTestResponsesOK, "input.0.content.0.text"},
		{PlatformDeepseek, APIProtocolAnthropic, "deepseek-v4-flash", modelTestClaudeOK, "messages.0.content.0.text"},
		{PlatformOpenCodeGo, APIProtocolResponses, "grok-4.3", modelTestResponsesOK, "input.0.content.0.text"},
		{PlatformOpenCodeGo, APIProtocolAnthropic, "claude-sonnet-4-6", modelTestClaudeOK, "messages.0.content.0.text"},
	} {
		t.Run(tc.platform+tc.protocol, func(t *testing.T) {
			account := modelTestAccount(tc.platform)
			account.Credentials["api_protocol"] = tc.protocol
			svc, upstream, _ := modelTestFixture(account, tc.stream)
			result, err := svc.RunModelTestBackground(context.Background(), 900, tc.model, "native-protocol-prompt", "")
			require.NoError(t, err)
			require.Equal(t, "success", result.Status, result.ErrorMessage)
			require.Equal(t, "native-protocol-prompt", gjson.GetBytes(upstream.lastBody, tc.promptPath).String())
		})
	}
}

// TestAccountModelTestFailedFinalRetainsText 保留仅在失败终帧携带的部分回答。
func TestAccountModelTestFailedFinalRetainsText(t *testing.T) {
	stream := "data: {\"type\":\"response.incomplete\",\"response\":{\"output\":[{\"content\":[{\"type\":\"output_text\",\"text\":\"partial\"}]}]}}\n\n"
	svc, _, _ := modelTestFixture(modelTestAccount(PlatformOpenAI), stream)
	result, err := svc.RunModelTestBackground(context.Background(), 900, "gpt-5.4", "prompt", "")
	require.NoError(t, err)
	require.Equal(t, "failed", result.Status)
	require.Equal(t, "partial", result.ResponseText)
}
