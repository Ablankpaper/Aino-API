//go:build integration && nativeconsumer

package repository_test

import (
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
	path, content                               string
	shutdown                                    chan struct{}
	shutdownOnce                                sync.Once
	toolResults, streamStarted, streamCancelled atomic.Int64
	streamShutdowns                             atomic.Int64
}

func (p *ainoNativeProtocol) closeActiveStream() {
	p.shutdownOnce.Do(func() { close(p.shutdown) })
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
		case <-req.Context().Done():
			p.streamCancelled.Add(1)
			return
		case <-p.shutdown:
			p.streamShutdowns.Add(1)
			return
		case <-time.After(25 * time.Second):
		}
	}
	emit(map[string]any{}, finish, usage)
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
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
