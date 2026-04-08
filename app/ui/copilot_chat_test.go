//go:build windows || darwin

package ui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ollama/ollama/app/store"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func installDefaultTransport(t *testing.T, transport http.RoundTripper) {
	t.Helper()
	original := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() {
		http.DefaultTransport = original
	})
}

func testCopilotChat() *store.Chat {
	chat := store.NewChat("chat-id")
	chat.Messages = append(chat.Messages,
		store.NewMessage("system", "You are terse.", nil),
		store.NewMessage("assistant", "Previous answer.", nil),
		store.NewMessage("user", "What now?", nil),
	)
	return chat
}

func testCopilotServer(t *testing.T) *Server {
	t.Helper()
	testStore := &store.Store{DBPath: filepath.Join(t.TempDir(), "db.sqlite")}
	t.Cleanup(func() {
		testStore.Close()
	})
	return &Server{Store: testStore}
}

func mustMap(t *testing.T, value any) map[string]any {
	t.Helper()
	mapped, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", value)
	}
	return mapped
}

func mustSlice(t *testing.T, value any) []any {
	t.Helper()
	slice, ok := value.([]any)
	if !ok {
		t.Fatalf("expected []any, got %T", value)
	}
	return slice
}

func TestCopilotPreferredChatEndpoint(t *testing.T) {
	tests := []struct {
		name      string
		meta      copilotModelMetadata
		wantRoute string
	}{
		{
			name:      "defaults to chat completions when no endpoints are declared",
			meta:      copilotModelMetadata{},
			wantRoute: "/chat/completions",
		},
		{
			name: "uses responses when it is the only supported endpoint",
			meta: copilotModelMetadata{SupportedEndpoints: []string{"/responses"}},
			wantRoute: "/responses",
		},
		{
			name: "prefers chat completions when both endpoints are supported",
			meta: copilotModelMetadata{SupportedEndpoints: []string{"/responses", "/chat/completions"}},
			wantRoute: "/chat/completions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := copilotPreferredChatEndpoint(tt.meta); got != tt.wantRoute {
				t.Fatalf("copilotPreferredChatEndpoint() = %q, want %q", got, tt.wantRoute)
			}
		})
	}
}

func TestStreamCopilotChatUsesResponsesEndpoint(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	var bodyErr error

	installDefaultTransport(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotPath = req.URL.Path
		payload, err := io.ReadAll(req.Body)
		if err != nil {
			bodyErr = err
		} else {
			bodyErr = json.Unmarshal(payload, &gotBody)
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
			},
			Body: io.NopCloser(strings.NewReader(`{"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)),
		}, nil
	}))

	server := testCopilotServer(t)
	session := &store.AuthSession{CopilotToken: "token"}
	meta := copilotModelMetadata{ID: "gpt-5.4-mini", SupportedEndpoints: []string{"/responses"}}

	var content strings.Builder
	if err := server.streamCopilotChat(context.Background(), session, meta, testCopilotChat(), "high", func(deltaContent, deltaThinking string) error {
		content.WriteString(deltaContent)
		return nil
	}); err != nil {
		t.Fatalf("streamCopilotChat() error = %v", err)
	}
	if bodyErr != nil {
		t.Fatalf("failed to inspect request body: %v", bodyErr)
	}
	if gotPath != "/responses" {
		t.Fatalf("request path = %q, want %q", gotPath, "/responses")
	}
	if content.String() != "ok" {
		t.Fatalf("streamed content = %q, want %q", content.String(), "ok")
	}
	if _, ok := gotBody["messages"]; ok {
		t.Fatal("responses request unexpectedly used chat-completions messages field")
	}
	if gotBody["instructions"] != "You are terse." {
		t.Fatalf("instructions = %#v, want %q", gotBody["instructions"], "You are terse.")
	}
	reasoning := mustMap(t, gotBody["reasoning"])
	if reasoning["summary"] != "auto" {
		t.Fatalf("reasoning summary = %#v, want %q", reasoning["summary"], "auto")
	}
	if reasoning["effort"] != "high" {
		t.Fatalf("reasoning effort = %#v, want %q", reasoning["effort"], "high")
	}
	input := mustSlice(t, gotBody["input"])
	if len(input) != 2 {
		t.Fatalf("input length = %d, want 2", len(input))
	}
	assistant := mustMap(t, input[0])
	if assistant["role"] != "assistant" {
		t.Fatalf("assistant role = %#v, want %q", assistant["role"], "assistant")
	}
	user := mustMap(t, input[1])
	if user["role"] != "user" {
		t.Fatalf("user role = %#v, want %q", user["role"], "user")
	}
}

func TestStreamCopilotChatUsesChatCompletionsEndpoint(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	var bodyErr error

	installDefaultTransport(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotPath = req.URL.Path
		payload, err := io.ReadAll(req.Body)
		if err != nil {
			bodyErr = err
		} else {
			bodyErr = json.Unmarshal(payload, &gotBody)
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
			},
			Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`)),
		}, nil
	}))

	server := testCopilotServer(t)
	session := &store.AuthSession{CopilotToken: "token"}
	meta := copilotModelMetadata{ID: "claude-sonnet-4.6", SupportedEndpoints: []string{"/responses", "/chat/completions"}}

	var content strings.Builder
	if err := server.streamCopilotChat(context.Background(), session, meta, testCopilotChat(), "none", func(deltaContent, deltaThinking string) error {
		content.WriteString(deltaContent)
		return nil
	}); err != nil {
		t.Fatalf("streamCopilotChat() error = %v", err)
	}
	if bodyErr != nil {
		t.Fatalf("failed to inspect request body: %v", bodyErr)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("request path = %q, want %q", gotPath, "/chat/completions")
	}
	if content.String() != "ok" {
		t.Fatalf("streamed content = %q, want %q", content.String(), "ok")
	}
	if _, ok := gotBody["input"]; ok {
		t.Fatal("chat completions request unexpectedly used responses input field")
	}
	if _, ok := gotBody["reasoning_effort"]; ok {
		t.Fatal("chat completions request unexpectedly sent reasoning_effort for think=none")
	}
	messages := mustSlice(t, gotBody["messages"])
	if len(messages) != 3 {
		t.Fatalf("messages length = %d, want 3", len(messages))
	}
}