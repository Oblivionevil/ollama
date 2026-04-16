//go:build windows || darwin || android

package ui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/app/store"
	"github.com/ollama/ollama/app/ui/responses"
)

type copilotChatDelta struct {
	Content          any `json:"content"`
	ReasoningContent any `json:"reasoning_content"`
}

type copilotChatChoice struct {
	Delta        copilotChatDelta `json:"delta"`
	Message      copilotChatDelta `json:"message"`
	FinishReason string           `json:"finish_reason"`
}

type copilotChatChunk struct {
	Choices []copilotChatChoice `json:"choices"`
}

type copilotResponseContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type copilotResponseSummaryPart struct {
	Text string `json:"text"`
}

type copilotResponseOutputItem struct {
	Type    string                       `json:"type"`
	Content []copilotResponseContentPart `json:"content"`
	Summary []copilotResponseSummaryPart `json:"summary"`
}

type copilotResponseEnvelope struct {
	Output []copilotResponseOutputItem `json:"output"`
}

type copilotResponseError struct {
	Message string `json:"message"`
}

type copilotResponseEvent struct {
	Type     string                  `json:"type"`
	Delta    string                  `json:"delta"`
	Response copilotResponseEnvelope `json:"response"`
	Error    *copilotResponseError   `json:"error"`
}

func copilotSupportsEndpoint(meta copilotModelMetadata, endpoint string) bool {
	for _, supported := range meta.SupportedEndpoints {
		if supported == endpoint {
			return true
		}
	}
	return false
}

func copilotPreferredChatEndpoint(meta copilotModelMetadata) string {
	if copilotSupportsEndpoint(meta, "/responses") && !copilotSupportsEndpoint(meta, "/chat/completions") {
		return "/responses"
	}
	return "/chat/completions"
}

func copilotReasoningEffort(think any) (string, bool) {
	switch value := think.(type) {
	case string:
		if value == "" {
			return "", false
		}
		return value, true
	case bool:
		if value {
			return "medium", true
		}
	}
	return "", false
}

func (s *Server) copilotMessageContent(message store.Message) any {
	if message.Role != "user" {
		content := firstNonEmpty(message.Content, message.Thinking)
		if content == "" {
			return nil
		}
		return content
	}

	text := strings.Builder{}
	if message.Content != "" {
		text.WriteString(message.Content)
	}

	parts := make([]map[string]any, 0, len(message.Attachments)+1)
	for _, attachment := range message.Attachments {
		if isImageAttachment(attachment.Filename) {
			mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(attachment.Filename)))
			if mimeType == "" {
				mimeType = "image/png"
			}
			parts = append(parts, map[string]any{
				"type": "image_url",
				"image_url": map[string]any{
					"url":    "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(attachment.Data),
					"detail": "auto",
				},
			})
			continue
		}

		if text.Len() > 0 {
			text.WriteString("\n")
		}
		text.WriteString("--- File: ")
		text.WriteString(attachment.Filename)
		text.WriteString(" ---\n")
		text.WriteString(convertBytesToText(attachment.Data, attachment.Filename))
		text.WriteString("\n--- End of ")
		text.WriteString(attachment.Filename)
		text.WriteString(" ---")
	}

	textContent := text.String()
	if len(parts) == 0 {
		return textContent
	}
	if textContent != "" {
		parts = append([]map[string]any{{
			"type": "text",
			"text": textContent,
		}}, parts...)
	}
	return parts
}

func (s *Server) copilotChatMessages(chat *store.Chat) []map[string]any {
	messages := make([]map[string]any, 0, len(chat.Messages))
	for _, message := range chat.Messages {
		if message.Role == "tool" {
			continue
		}
		if message.Role != "user" && message.Role != "assistant" && message.Role != "system" {
			continue
		}

		content := s.copilotMessageContent(message)
		if content == nil {
			continue
		}

		messages = append(messages, map[string]any{
			"role":    message.Role,
			"content": content,
		})
	}
	return messages
}

func (s *Server) copilotChatCompletionsRequestBody(chat *store.Chat, meta copilotModelMetadata, think any) map[string]any {
	body := map[string]any{
		"model":    meta.ID,
		"messages": s.copilotChatMessages(chat),
		"stream":   true,
		"stream_options": map[string]any{
			"include_usage": true,
		},
	}

	if effort, ok := copilotReasoningEffort(think); ok && effort != "none" {
		body["reasoning_effort"] = effort
	}

	return body
}

func (s *Server) copilotResponsesUserContentParts(content any) []map[string]any {
	switch typed := content.(type) {
	case string:
		if typed == "" {
			return nil
		}
		return []map[string]any{{
			"type": "input_text",
			"text": typed,
		}}
	case []map[string]any:
		parts := make([]map[string]any, 0, len(typed))
		for _, part := range typed {
			switch part["type"] {
			case "text":
				text, _ := part["text"].(string)
				if text == "" {
					continue
				}
				parts = append(parts, map[string]any{
					"type": "input_text",
					"text": text,
				})
			case "image_url":
				imageURL, _ := part["image_url"].(map[string]any)
				url, _ := imageURL["url"].(string)
				if url == "" {
					continue
				}
				detail, _ := imageURL["detail"].(string)
				if detail == "" {
					detail = "auto"
				}
				parts = append(parts, map[string]any{
					"type":      "input_image",
					"image_url": url,
					"detail":    detail,
				})
			}
		}
		return parts
	}

	return nil
}

func (s *Server) copilotResponsesInput(chat *store.Chat) (string, []map[string]any) {
	instructions := make([]string, 0)
	input := make([]map[string]any, 0, len(chat.Messages))

	for _, message := range chat.Messages {
		if message.Role == "tool" {
			continue
		}

		content := s.copilotMessageContent(message)
		if content == nil {
			continue
		}

		switch message.Role {
		case "system":
			text := extractCopilotText(content)
			if text != "" {
				instructions = append(instructions, text)
			}
		case "assistant":
			text := extractCopilotText(content)
			if text == "" {
				continue
			}
			input = append(input, map[string]any{
				"type": "message",
				"id":   "msg_" + uuid.NewString(),
				"role": "assistant",
				"content": []map[string]any{{
					"type":        "output_text",
					"text":        text,
					"annotations": []any{},
				}},
				"status": "completed",
			})
		case "user":
			parts := s.copilotResponsesUserContentParts(content)
			if len(parts) == 0 {
				continue
			}
			input = append(input, map[string]any{
				"type":    "message",
				"role":    "user",
				"content": parts,
			})
		}
	}

	return strings.Join(instructions, "\n"), input
}

func (s *Server) copilotResponsesRequestBody(chat *store.Chat, meta copilotModelMetadata, think any) map[string]any {
	instructions, input := s.copilotResponsesInput(chat)
	body := map[string]any{
		"model":   meta.ID,
		"input":   input,
		"store":   false,
		"stream":  true,
		"include": []string{"reasoning.encrypted_content"},
	}
	if instructions != "" {
		body["instructions"] = instructions
	}
	if effort, ok := copilotReasoningEffort(think); ok {
		body["reasoning"] = map[string]any{
			"summary": "auto",
			"effort":  effort,
		}
	}
	return body
}

func extractCopilotText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		var builder strings.Builder
		for _, item := range typed {
			builder.WriteString(extractCopilotText(item))
		}
		return builder.String()
	case map[string]any:
		if text, ok := typed["text"].(string); ok {
			return text
		}
		if delta, ok := typed["delta"].(string); ok {
			return delta
		}
	}
	return ""
}

func extractCopilotResponseOutput(output []copilotResponseOutputItem) (string, string) {
	var content strings.Builder
	var thinking strings.Builder

	for _, item := range output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" && part.Text != "" {
					content.WriteString(part.Text)
				}
			}
		case "reasoning":
			for _, summary := range item.Summary {
				if summary.Text == "" {
					continue
				}
				if thinking.Len() > 0 {
					thinking.WriteString("\n\n")
				}
				thinking.WriteString(summary.Text)
			}
		}
	}

	return content.String(), thinking.String()
}

func (s *Server) doCopilotChatRequest(ctx context.Context, session *store.AuthSession, meta copilotModelMetadata, endpoint string, body map[string]any) (*http.Response, error) {
	requestID := uuid.NewString()
	headers := s.copilotRequestHeaders(session.CopilotToken, "conversation-panel", requestID, s.copilotRequestModelHeaders(meta))
	headers["Content-Type"] = "application/json"

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.githubcopilot.com"+endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create chat request: %w", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := s.copilotChatHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		s.copilotClearAuthSession()
		return nil, api.AuthorizationError{StatusCode: http.StatusUnauthorized, Status: resp.Status}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("copilot chat failed: %s", strings.TrimSpace(string(body)))
	}

	return resp, nil
}

func (s *Server) streamCopilotChatCompletions(ctx context.Context, session *store.AuthSession, meta copilotModelMetadata, chat *store.Chat, think any, onDelta func(content, thinking string) error) error {
	resp, err := s.doCopilotChatRequest(ctx, session, meta, "/chat/completions", s.copilotChatCompletionsRequestBody(chat, meta, think))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/event-stream") {
		body, _ := io.ReadAll(resp.Body)
		var chunk copilotChatChunk
		if err := json.Unmarshal(body, &chunk); err != nil {
			return fmt.Errorf("parse chat response: %w", err)
		}
		for _, choice := range chunk.Choices {
			if err := onDelta(extractCopilotText(choice.Message.Content), extractCopilotText(choice.Message.ReasoningContent)); err != nil {
				return err
			}
		}
		return nil
	}

	reader := bufio.NewReader(resp.Body)
	var dataLines []string
	flushChunk := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if payload == "[DONE]" {
			return io.EOF
		}

		var chunk copilotChatChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return fmt.Errorf("parse chat event: %w", err)
		}
		for _, choice := range chunk.Choices {
			if err := onDelta(extractCopilotText(choice.Delta.Content), extractCopilotText(choice.Delta.ReasoningContent)); err != nil {
				return err
			}
		}
		return nil
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}

		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
		case trimmed == "":
			flushErr := flushChunk()
			if errors.Is(flushErr, io.EOF) {
				return nil
			}
			if flushErr != nil {
				return flushErr
			}
		}

		if errors.Is(err, io.EOF) {
			break
		}
	}

	flushErr := flushChunk()
	if errors.Is(flushErr, io.EOF) {
		return nil
	}
	return flushErr
}

func (s *Server) streamCopilotResponses(ctx context.Context, session *store.AuthSession, meta copilotModelMetadata, chat *store.Chat, think any, onDelta func(content, thinking string) error) error {
	resp, err := s.doCopilotChatRequest(ctx, session, meta, "/responses", s.copilotResponsesRequestBody(chat, meta, think))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/event-stream") {
		body, _ := io.ReadAll(resp.Body)
		var response copilotResponseEnvelope
		if err := json.Unmarshal(body, &response); err != nil {
			return fmt.Errorf("parse responses response: %w", err)
		}
		content, thinking := extractCopilotResponseOutput(response.Output)
		if content == "" && thinking == "" {
			return nil
		}
		return onDelta(content, thinking)
	}

	reader := bufio.NewReader(resp.Body)
	var dataLines []string
	sawContent := false
	sawThinking := false
	flushChunk := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if payload == "[DONE]" {
			return io.EOF
		}

		var event copilotResponseEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return fmt.Errorf("parse responses event: %w", err)
		}

		switch event.Type {
		case "response.output_text.delta":
			if event.Delta == "" {
				return nil
			}
			sawContent = true
			return onDelta(event.Delta, "")
		case "response.reasoning_summary_text.delta":
			if event.Delta == "" {
				return nil
			}
			sawThinking = true
			return onDelta("", event.Delta)
		case "response.failed", "error":
			if event.Error != nil && event.Error.Message != "" {
				return errors.New(event.Error.Message)
			}
		case "response.completed":
			content, thinking := extractCopilotResponseOutput(event.Response.Output)
			if sawContent {
				content = ""
			}
			if sawThinking {
				thinking = ""
			}
			if content != "" || thinking != "" {
				return onDelta(content, thinking)
			}
		}

		return nil
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}

		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
		case trimmed == "":
			flushErr := flushChunk()
			if errors.Is(flushErr, io.EOF) {
				return nil
			}
			if flushErr != nil {
				return flushErr
			}
		}

		if errors.Is(err, io.EOF) {
			break
		}
	}

	flushErr := flushChunk()
	if errors.Is(flushErr, io.EOF) {
		return nil
	}
	return flushErr
}

func (s *Server) streamCopilotChat(ctx context.Context, session *store.AuthSession, meta copilotModelMetadata, chat *store.Chat, think any, onDelta func(content, thinking string) error) error {
	switch copilotPreferredChatEndpoint(meta) {
	case "/responses":
		return s.streamCopilotResponses(ctx, session, meta, chat, think, onDelta)
	default:
		return s.streamCopilotChatCompletions(ctx, session, meta, chat, think, onDelta)
	}
}

func (s *Server) chatCopilot(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, session *store.AuthSession, chat *store.Chat, req responses.ChatRequest, meta *copilotModelMetadata) error {
	var thinkingTimeStart *time.Time
	var thinkingTimeEnd *time.Time
	assistantCreated := false

	ensureAssistant := func() error {
		if assistantCreated && len(chat.Messages) > 0 && chat.Messages[len(chat.Messages)-1].Role == "assistant" {
			return nil
		}

		message := store.NewMessage("assistant", "", &store.MessageOptions{Model: req.Model, Stream: true})
		chat.Messages = append(chat.Messages, message)
		assistantCreated = true
		return nil
	}

	appendThinking := func(delta string) error {
		if delta == "" {
			return nil
		}
		// Filter out placeholder dots that some models return instead of
		// actual reasoning content (e.g. "..." from /responses summaries).
		if strings.Trim(delta, ".… \t\n\r") == "" {
			return nil
		}
		if thinkingTimeStart == nil || thinkingTimeEnd != nil {
			now := time.Now()
			thinkingTimeStart = &now
			thinkingTimeEnd = nil
		}
		if err := ensureAssistant(); err != nil {
			return err
		}
		last := &chat.Messages[len(chat.Messages)-1]
		last.Thinking += delta
		last.UpdatedAt = time.Now()
		last.ThinkingTimeStart = thinkingTimeStart
		if thinkingTimeEnd != nil {
			last.ThinkingTimeEnd = thinkingTimeEnd
		}
		if err := json.NewEncoder(w).Encode(responses.ChatEvent{
			EventName:         "thinking",
			Thinking:          &delta,
			ThinkingTimeStart: thinkingTimeStart,
			ThinkingTimeEnd:   thinkingTimeEnd,
		}); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	appendContent := func(delta string) error {
		if delta == "" {
			return nil
		}
		if thinkingTimeStart != nil && thinkingTimeEnd == nil {
			now := time.Now()
			thinkingTimeEnd = &now
		}
		if err := ensureAssistant(); err != nil {
			return err
		}
		last := &chat.Messages[len(chat.Messages)-1]
		last.Content += delta
		last.UpdatedAt = time.Now()
		if thinkingTimeStart != nil {
			last.ThinkingTimeStart = thinkingTimeStart
		}
		if thinkingTimeEnd != nil {
			last.ThinkingTimeEnd = thinkingTimeEnd
		}
		if err := json.NewEncoder(w).Encode(responses.ChatEvent{
			EventName:         "chat",
			Content:           &delta,
			ThinkingTimeStart: thinkingTimeStart,
			ThinkingTimeEnd:   thinkingTimeEnd,
		}); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	err := s.streamCopilotChat(ctx, session, *meta, chat, req.Think, func(content, thinking string) error {
		if err := appendThinking(thinking); err != nil {
			return err
		}
		if err := appendContent(content); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		s.log().Error("copilot chat stream error", "error", err)
		if len(chat.Messages) > 0 && chat.Messages[len(chat.Messages)-1].Role == "assistant" {
			chat.Messages[len(chat.Messages)-1].Stream = false
		}
		if persistErr := s.storeChatRemotely(session, *chat); persistErr != nil {
			s.log().Warn("failed to persist partial chat to GitHub", "chat_id", chat.ID, "error", persistErr)
		}
		errorEvent := s.getError(err)
		json.NewEncoder(w).Encode(errorEvent)
		flusher.Flush()
		return nil
	}

	if thinkingTimeStart != nil && thinkingTimeEnd == nil && len(chat.Messages) > 0 && chat.Messages[len(chat.Messages)-1].Role == "assistant" {
		now := time.Now()
		thinkingTimeEnd = &now
		last := &chat.Messages[len(chat.Messages)-1]
		last.ThinkingTimeEnd = thinkingTimeEnd
		last.UpdatedAt = now
	}

	if len(chat.Messages) > 0 && chat.Messages[len(chat.Messages)-1].Role == "assistant" {
		chat.Messages[len(chat.Messages)-1].Stream = false
	}

	if err := s.storeChatRemotely(session, *chat); err != nil {
		return err
	}

	if err := json.NewEncoder(w).Encode(responses.ChatEvent{EventName: "done"}); err != nil {
		return err
	}
	flusher.Flush()

	return nil
}
