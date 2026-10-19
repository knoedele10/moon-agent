package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Exercise the real OpenAI-compatible HTTP component rather than replacing it
// with a mock model. No request reaches xAI and the key is deliberately fake.
func TestGrokStreamingAndToolTransport(t *testing.T) {
	for _, withTool := range []bool{false, true} {
		t.Run(fmt.Sprintf("tools=%v", withTool), func(t *testing.T) {
			var seenModel, seenAuth, seenPath string
			var seenStream bool
			var toolCount int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seenAuth, seenPath = r.Header.Get("Authorization"), r.URL.Path
				var body struct {
					Model  string            `json:"model"`
					Stream bool              `json:"stream"`
					Tools  []json.RawMessage `json:"tools"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					http.Error(w, "invalid request", 400)
					return
				}
				seenModel, seenStream, toolCount = body.Model, body.Stream, len(body.Tools)
				w.Header().Set("Content-Type", "text/event-stream")
				if withTool {
					fmt.Fprint(w, "data: {\"id\":\"grok-test\",\"object\":\"chat.completion.chunk\",\"model\":\"grok-4.7\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"read-1\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"file_path\\\":\\\"a.txt\\\"}\"}}]},\"finish_reason\":null}]}\n\n")
				} else {
					fmt.Fprint(w, "data: {\"id\":\"grok-test\",\"object\":\"chat.completion.chunk\",\"model\":\"grok-4.7\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好，Grok\"},\"finish_reason\":null}]}\n\n")
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer srv.Close()
			reg := NewRegistry()
			reg.Register("openai", NewOpenAIBackend)
			eps := DefaultEndpoints()
			found := false
			for i := range eps {
				if eps[i].ID == "xai" {
					if eps[i].EnvKey != "XAI_API_KEY" || eps[i].BaseURL != "https://api.x.ai/v1" {
						t.Fatal("incorrect official endpoint configuration")
					}
					eps[i].BaseURL = srv.URL + "/v1"
					found = true
				}
			}
			if !found {
				t.Fatal("Grok endpoint missing")
			}
			reg.SetEndpoints(eps)
			req := ChatRequest{Messages: []Message{{Role: RoleUser, Content: "你好"}}}
			if withTool {
				req.Tools = []ToolDef{{Name: "read", Description: "读取文件", Parameters: map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}}}}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch, err := reg.Stream(ctx, "grok-4.7", "test-xai-key", req)
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			var calls []ToolCall
			done := false
			for ev := range ch {
				switch ev.Type {
				case EventDelta:
					output.WriteString(ev.Content)
				case EventToolCall:
					if ev.ToolCall != nil {
						calls = append(calls, *ev.ToolCall)
					}
				case EventError:
					t.Fatal(ev.Err)
				case EventDone:
					done = true
				}
			}
			if !done || seenModel != "grok-4.7" || !seenStream || seenPath != "/v1/chat/completions" || seenAuth != "Bearer test-xai-key" {
				t.Fatalf("bad transport: model=%q stream=%v path=%q done=%v", seenModel, seenStream, seenPath, done)
			}
			if withTool {
				if toolCount != 1 || len(calls) != 1 || calls[0].Name != "read" || calls[0].Arguments != `{"file_path":"a.txt"}` {
					t.Fatalf("tool transport lost fields: %+v", calls)
				}
			} else if output.String() != "你好，Grok" {
				t.Fatalf("bad streamed text: %q", output.String())
			}
		})
	}
}
