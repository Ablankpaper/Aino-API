//go:build integration && nativeconsumer

package repository_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Protocol-only provider: a real Agent must choose/execute read_file, and the
// second request must carry its matching assistant call and actual file result.
type ainoNativeProtocol struct {
	rig                                         *ainoPlatformFixture
	faults                                      *ainoNativeFaults
	path, content                               string
	shutdown, releaseTerminal                   chan struct{}
	shutdownOnce, releaseTerminalOnce           sync.Once
	terminalReleaseTimeout                      time.Duration
	toolResults, streamStarted, streamCancelled atomic.Int64
	downstreamDisconnects, streamDrained        atomic.Int64
	streamTimeouts                              atomic.Int64
	streamShutdowns                             atomic.Int64
	compressionRequests                         atomic.Int64
	compressionHandoffRequests                  atomic.Int64
}

func (p *ainoNativeProtocol) closeActiveStream() {
	p.shutdownOnce.Do(func() { close(p.shutdown) })
}

func (p *ainoNativeProtocol) releaseTerminalStream() {
	p.releaseTerminalOnce.Do(func() { close(p.releaseTerminal) })
}

func (p *ainoNativeProtocol) watchNativeDownstreamRequest(req *http.Request) func() {
	if !isNativeCancellationRequest(req) {
		return func() {}
	}
	handlerFinished := make(chan struct{})
	var finishOnce sync.Once
	go func() {
		select {
		case <-req.Context().Done():
			select {
			case <-handlerFinished:
				return
			default:
			}
			p.downstreamDisconnects.Add(1)
			p.releaseTerminalStream()
		case <-handlerFinished:
		}
	}()
	return func() { finishOnce.Do(func() { close(handlerFinished) }) }
}

func isNativeCancellationRequest(req *http.Request) bool {
	if req.Method != http.MethodPost || req.URL.Path != "/v1/chat/completions" || req.Body == nil {
		return false
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return false
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	var payload struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	for index := len(payload.Messages) - 1; index >= 0; index-- {
		message := payload.Messages[index]
		if message.Role == "user" {
			return strings.Contains(string(message.Content), "fixture-cancel-stream")
		}
	}
	return false
}

func (p *ainoNativeProtocol) terminalReleaseDeadline() time.Duration {
	if p.terminalReleaseTimeout > 0 {
		return p.terminalReleaseTimeout
	}
	return 25 * time.Second
}

func registerNativeProtocolCleanup(t *testing.T, protocol *ainoNativeProtocol, closeServer func()) {
	t.Helper()
	// Go runs cleanup handlers in LIFO order, so register the server first.
	t.Cleanup(closeServer)
	t.Cleanup(protocol.closeActiveStream)
}

func (p *ainoNativeProtocol) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/v1/chat/completions" || req.Header.Get("Authorization") != "Bearer fixture-upstream-key" {
		http.Error(w, "unexpected fixture upstream request", 400)
		return
	}
	for name := range req.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-aino-") {
			http.Error(w, "private attribution forwarded", 400)
			return
		}
	}
	var body struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name       string         `json:"name"`
				Parameters map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if json.NewDecoder(req.Body).Decode(&body) != nil || body.Model != "fixture-tool-model" || len(body.Messages) == 0 {
		http.Error(w, "invalid native fixture protocol", 400)
		return
	}
	for _, msg := range body.Messages {
		if strings.Contains(string(msg.Content), "CONTEXT COMPACTION") {
			p.compressionHandoffRequests.Add(1)
		}
	}
	if len(body.Tools) == 0 {
		if !isNativeCompressionRequest(body.Messages) {
			http.Error(w, "unknown no-tool native fixture request", 400)
			return
		}
		p.compressionRequests.Add(1)
		call := p.rig.modelCalls.Add(1)
		message := map[string]any{"role": "assistant", "content": "## Historical Task\nNative compression fixture handoff preserved the prior tool turns.\n\n## Active State\nThe resumed native chat remains bound to the same billing session."}
		usage := map[string]int{"prompt_tokens": 18, "completion_tokens": 8, "total_tokens": 26}
		w.Header().Set("x-request-id", fmt.Sprintf("fixture-%s-%d", p.rig.runID, call))
		if !body.Stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("fixture-%d", call), "object": "chat.completion", "created": time.Now().Unix(), "model": body.Model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": "stop"}}, "usage": usage})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "fixture streaming unsupported", http.StatusInternalServerError)
			return
		}
		emit := func(delta map[string]any, reason any, counts any) {
			blob, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("fixture-%d", call), "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": body.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}, "usage": counts})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", blob)
			flusher.Flush()
		}
		emit(message, nil, nil)
		emit(map[string]any{}, "stop", usage)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}
	hasRead := false
	for _, tool := range body.Tools {
		if tool.Function.Name == "read_file" {
			props, _ := tool.Function.Parameters["properties"].(map[string]any)
			_, hasRead = props["path"]
		}
	}
	if !hasRead {
		http.Error(w, "actual read_file tool definition missing", 400)
		return
	}
	if p.faults != nil && p.faults.providerAttempt(w) {
		return
	}
	call := p.rig.modelCalls.Add(1)
	last := body.Messages[len(body.Messages)-1]
	toolID := "fixture-native-read"
	arguments, _ := json.Marshal(map[string]string{"path": p.path})
	message := map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": toolID, "type": "function", "function": map[string]any{"name": "read_file", "arguments": string(arguments)}}}}
	finish := "tool_calls"
	var lastUser string
	for _, msg := range body.Messages {
		if msg.Role == "user" {
			lastUser = string(msg.Content)
		}
	}
	cancel := strings.Contains(lastUser, "fixture-cancel-stream")
	if last.Role == "tool" {
		matched := false
		for index := len(body.Messages) - 2; index >= 0; index-- {
			msg := body.Messages[index]
			if msg.Role == "tool" {
				continue
			}
			if msg.Role != "assistant" {
				break
			}
			for _, tool := range msg.ToolCalls {
				if tool.ID == last.ToolCallID && tool.Function.Name == "read_file" {
					var args map[string]string
					if json.Unmarshal([]byte(tool.Function.Arguments), &args) == nil && args["path"] == p.path {
						matched = true
					}
				}
			}
			break
		}
		if last.ToolCallID != toolID || !matched || !strings.Contains(string(last.Content), p.content) {
			http.Error(w, "native tool roundtrip mismatch", 400)
			return
		}
		p.toolResults.Add(1)
		message = map[string]any{"role": "assistant", "content": "Verified " + p.content}
		finish = "stop"
	}
	if cancel {
		message = map[string]any{"role": "assistant", "content": "Fixture stream started"}
		finish = "stop"
	}
	w.Header().Set("x-request-id", fmt.Sprintf("fixture-%s-%d", p.rig.runID, call))
	usage := map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	if !body.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("fixture-%d", call), "object": "chat.completion", "created": time.Now().Unix(), "model": body.Model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": usage})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "fixture streaming unsupported", http.StatusInternalServerError)
		return
	}
	emit := func(delta map[string]any, reason any, counts any) {
		blob, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("fixture-%d", call), "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": body.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}, "usage": counts})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", blob)
		flusher.Flush()
	}
	if calls, ok := message["tool_calls"].([]any); ok {
		call, ok := calls[0].(map[string]any)
		if !ok {
			http.Error(w, "fixture tool call malformed", http.StatusInternalServerError)
			return
		}
		call["index"] = 0
	}
	emit(message, nil, nil)
	if cancel {
		p.streamStarted.Add(1)
		select {
		case <-p.releaseTerminal:
			// The downstream request has ended; give the raw relay one more
			// write to observe it, then provide the finite usage tail to drain.
			emit(map[string]any{}, nil, nil)
		case <-req.Context().Done():
			p.streamCancelled.Add(1)
			return
		case <-p.shutdown:
			p.streamShutdowns.Add(1)
			return
		case <-time.After(p.terminalReleaseDeadline()):
			p.streamTimeouts.Add(1)
			return
		}
	}
	emit(map[string]any{}, finish, usage)
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
	if cancel {
		p.streamDrained.Add(1)
	}
}

func isNativeCompressionRequest(messages []struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}) bool {
	if len(messages) != 1 || messages[0].Role != "user" {
		return false
	}
	var content string
	if json.Unmarshal(messages[0].Content, &content) != nil {
		return false
	}
	const prefix = "You are a summarization agent creating a context checkpoint."
	const marker = "TURNS TO SUMMARIZE:"
	if !strings.HasPrefix(content, prefix) {
		return false
	}
	markerAt := strings.Index(content[len(prefix):], marker)
	if markerAt < 0 {
		return false
	}
	context := content[len(prefix)+markerAt+len(marker):]
	if boundary := strings.Index(context, "\n\nUse this exact structure:"); boundary >= 0 {
		context = context[:boundary]
	}
	return strings.TrimSpace(context) != ""
}

func TestAinoNativeProtocolAcceptsCompressionSummaryRequest(t *testing.T) {
	protocol := &ainoNativeProtocol{rig: &ainoPlatformFixture{runID: "native-compression-test"}}
	server := httptest.NewServer(protocol)
	defer server.Close()
	body := `{"model":"fixture-tool-model","stream":false,"messages":[{"role":"user","content":"You are a summarization agent creating a context checkpoint. TURNS TO SUMMARIZE: fixture context"}]}`
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer fixture-upstream-key")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Contains(t, string(data), "Native compression fixture handoff")
	require.EqualValues(t, 1, protocol.compressionRequests.Load())
}

func TestAinoNativeProtocolRejectsMalformedNoToolRequest(t *testing.T) {
	for name, content := range map[string]string{
		"ordinary chat": "ordinary chat without tools",
		"prefix only":   "You are a summarization agent creating a context checkpoint.",
		"empty context": "You are a summarization agent creating a context checkpoint.\n\nTURNS TO SUMMARIZE:\n\nUse this exact structure:\nfixture template",
	} {
		t.Run(name, func(t *testing.T) {
			protocol := &ainoNativeProtocol{rig: &ainoPlatformFixture{runID: "native-compression-malformed-test"}}
			server := httptest.NewServer(protocol)
			defer server.Close()
			encoded, err := json.Marshal(map[string]any{"model": "fixture-tool-model", "stream": false, "messages": []any{map[string]any{"role": "user", "content": content}}})
			require.NoError(t, err)
			request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(encoded))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer fixture-upstream-key")
			request.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(request)
			require.NoError(t, err)
			defer response.Body.Close()
			require.Equal(t, http.StatusBadRequest, response.StatusCode)
			require.EqualValues(t, 0, protocol.compressionRequests.Load())
		})
	}
}

func TestAinoNativeProtocolShutdownDoesNotCountAsCancellation(t *testing.T) {
	protocol := &ainoNativeProtocol{
		rig:      &ainoPlatformFixture{runID: "native-shutdown-test"},
		path:     "/tmp/native-shutdown-test.txt",
		content:  "native-shutdown-test-content",
		shutdown: make(chan struct{}),
	}
	server := httptest.NewServer(protocol)
	defer server.Close()
	body := `{"model":"fixture-tool-model","stream":true,"messages":[{"role":"user","content":"fixture-cancel-stream"}],"tools":[{"function":{"name":"read_file","parameters":{"properties":{"path":{}}}}}]}`
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer fixture-upstream-key")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Eventually(t, func() bool { return protocol.streamStarted.Load() == 1 }, time.Second, 10*time.Millisecond)

	protocol.closeActiveStream()
	_, err = io.ReadAll(response.Body)
	require.NoError(t, err)
	require.EqualValues(t, 0, protocol.streamCancelled.Load())
	require.EqualValues(t, 1, protocol.streamShutdowns.Load())
}

func TestNativeProtocolReleasesTerminalOnlyAfterSelectedDownstreamDisconnect(t *testing.T) {
	protocol := &ainoNativeProtocol{
		shutdown:        make(chan struct{}),
		releaseTerminal: make(chan struct{}),
	}
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		finishWatching := protocol.watchNativeDownstreamRequest(req)
		defer finishWatching()
		close(entered)
		select {
		case <-protocol.releaseTerminal:
			w.WriteHeader(http.StatusNoContent)
		case <-protocol.shutdown:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"fixture-cancel-stream"}]}`))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	requestDone := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if response != nil {
			response.Body.Close()
		}
		requestDone <- err
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("native fixture never received the selected downstream request")
	}
	require.EqualValues(t, 0, protocol.downstreamDisconnects.Load())
	select {
	case <-protocol.releaseTerminal:
		t.Fatal("fixture released terminal stream before downstream disconnect")
	default:
	}

	cancel()
	require.Eventually(t, func() bool { return protocol.downstreamDisconnects.Load() == 1 }, time.Second, 10*time.Millisecond)
	select {
	case <-protocol.releaseTerminal:
	case <-time.After(time.Second):
		t.Fatal("fixture did not release terminal stream after downstream disconnect")
	}
	require.Error(t, <-requestDone)
}

func TestNativeProtocolOnlySelectsLatestUserCancellationMarker(t *testing.T) {
	protocol := &ainoNativeProtocol{shutdown: make(chan struct{}), releaseTerminal: make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		finishWatching := protocol.watchNativeDownstreamRequest(req)
		defer finishWatching()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"fixture-cancel-stream"},{"role":"assistant","content":"interrupted"},{"role":"user","content":"normal restored turn"}]}`))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	response.Body.Close()

	require.Never(t, func() bool { return protocol.downstreamDisconnects.Load() != 0 }, 100*time.Millisecond, 10*time.Millisecond)
	select {
	case <-protocol.releaseTerminal:
		t.Fatal("historical cancellation marker released terminal stream")
	default:
	}
}

func TestNativeProtocolNormalCompletionDoesNotCountAsDownstreamDisconnect(t *testing.T) {
	protocol := &ainoNativeProtocol{shutdown: make(chan struct{}), releaseTerminal: make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		finishWatching := protocol.watchNativeDownstreamRequest(req)
		finishWatching()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"fixture-cancel-stream"}]}`))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	response.Body.Close()

	require.Never(t, func() bool { return protocol.downstreamDisconnects.Load() != 0 }, 100*time.Millisecond, 10*time.Millisecond)
	select {
	case <-protocol.releaseTerminal:
		t.Fatal("normal completion released terminal stream")
	default:
	}
}

func TestNativeProtocolDeadlineDoesNotEmitTerminalUsage(t *testing.T) {
	protocol := &ainoNativeProtocol{
		rig:                    &ainoPlatformFixture{runID: "native-timeout-test"},
		path:                   "/tmp/native-timeout-test.txt",
		content:                "native-timeout-test-content",
		shutdown:               make(chan struct{}),
		releaseTerminal:        make(chan struct{}),
		terminalReleaseTimeout: 10 * time.Millisecond,
	}
	server := httptest.NewServer(protocol)
	defer server.Close()
	body := `{"model":"fixture-tool-model","stream":true,"messages":[{"role":"user","content":"fixture-cancel-stream"}],"tools":[{"function":{"name":"read_file","parameters":{"properties":{"path":{}}}}}]}`
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer fixture-upstream-key")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	stream, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Contains(t, string(stream), "Fixture stream started")
	require.NotContains(t, string(stream), "[DONE]")
	require.EqualValues(t, 1, protocol.streamTimeouts.Load())
	require.EqualValues(t, 0, protocol.streamDrained.Load())
}

func TestRegisterNativeProtocolCleanupSignalsShutdownBeforeServerClose(t *testing.T) {
	protocol := &ainoNativeProtocol{shutdown: make(chan struct{})}
	serverClosedAfterShutdown := false

	t.Run("cleanup", func(t *testing.T) {
		registerNativeProtocolCleanup(t, protocol, func() {
			select {
			case <-protocol.shutdown:
				serverClosedAfterShutdown = true
			default:
				t.Error("server close ran before native stream shutdown")
			}
		})
	})

	require.True(t, serverClosedAfterShutdown)
}
