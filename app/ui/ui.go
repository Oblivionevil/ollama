//go:build windows || darwin || android

// package ui implements a chat interface for Ollama
package ui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/app/store"
	"github.com/ollama/ollama/app/tools"
	"github.com/ollama/ollama/app/types/not"
	"github.com/ollama/ollama/app/ui/responses"
	"github.com/ollama/ollama/app/version"
	"github.com/ollama/ollama/envconfig"
	_ "github.com/tkrajina/typescriptify-golang-structs/typescriptify"
)

//go:generate tscriptify -package=github.com/ollama/ollama/app/ui/responses -target=./app/codegen/gotypes.gen.ts responses/types.go
//go:generate npm --prefix ./app run build

var CORS = envconfig.Bool("OLLAMA_CORS")

// OllamaDotCom returns the URL for ollama.com, allowing override via environment variable
var OllamaDotCom = func() string {
	if url := os.Getenv("OLLAMA_DOT_COM_URL"); url != "" {
		return url
	}
	return "https://ollama.com"
}()

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) Written() bool {
	return r.code != 0
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Status() int {
	if r.code == 0 {
		return http.StatusOK
	}
	return r.code
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Event is a string that represents the type of event being sent to the
// client. It is used in the Server-Sent Events (SSE) protocol to identify
// the type of data being sent.
// The client (template) will use this type in the sse event listener to
// determine how to handle the incoming data. It will also be used in the
// sse-swap htmx event listener to determine how to handle the incoming data.
type Event string

const (
	EventChat       Event = "chat"
	EventComplete   Event = "complete"
	EventLoading    Event = "loading"
	EventToolResult Event = "tool_result" // Used for both tool calls and their results
	EventThinking   Event = "thinking"
	EventToolCall   Event = "tool_call"
	EventDownload   Event = "download"
)

type Server struct {
	Logger           *slog.Logger
	Restart          func()
	Token            string
	Store            *store.Store
	githubChatRepo   string
	githubAPIBaseURL string
	ToolRegistry     *tools.Registry
	Tools            bool   // if true, the server will use single-turn tools to fulfill the user's request
	WebSearch        bool   // if true, the server will use single-turn browser tool to fulfill the user's request
	Agent            bool   // if true, the server will use multi-turn tools to fulfill the user's request
	WorkingDir       string // Working directory for all agent operations

	// Dev is true if the server is running in development mode
	Dev bool

	copilotStateMu        sync.Mutex
	copilotSessionIDValue string
	copilotFlows          map[string]*copilotDeviceFlow
	copilotModelCache     []copilotModelMetadata
	copilotModelCacheAt   time.Time
}

func (s *Server) log() *slog.Logger {
	if s.Logger == nil {
		return slog.Default()
	}
	return s.Logger
}

func (s *Server) apiVersion(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]string{"version": version.Version})
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(api.StatusResponse{
		Cloud: api.CloudStatus{Disabled: false, Source: "copilot"},
	})
}

type errHandlerFunc func(http.ResponseWriter, *http.Request) error

func (s *Server) Handler() http.Handler {
	handle := func(f errHandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Add CORS headers for dev work
			if CORS() {
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, User-Agent, Accept, X-Requested-With")
				w.Header().Set("Access-Control-Allow-Credentials", "true")

				// Handle preflight requests
				if r.Method == "OPTIONS" {
					w.WriteHeader(http.StatusOK)
					return
				}
			}

			// Don't check for token in development mode
			if !s.Dev {
				if token := r.URL.Query().Get("token"); token != "" && token == s.Token {
					query := r.URL.Query()
					query.Del("token")

					redirectURL := *r.URL
					redirectURL.RawQuery = query.Encode()
					location := redirectURL.String()
					if location == "" {
						location = "/"
					}

					http.SetCookie(w, &http.Cookie{
						Name:     "token",
						Value:    s.Token,
						Path:     "/",
						HttpOnly: true,
						SameSite: http.SameSiteStrictMode,
					})
					http.Redirect(w, r, location, http.StatusFound)
					return
				}

				cookie, err := r.Cookie("token")
				if err != nil {
					w.WriteHeader(http.StatusForbidden)
					json.NewEncoder(w).Encode(map[string]string{"error": "Token is required"})
					return
				}

				if cookie.Value != s.Token {
					w.WriteHeader(http.StatusForbidden)
					json.NewEncoder(w).Encode(map[string]string{"error": "Token is required"})
					return
				}
			}

			sw := &statusRecorder{ResponseWriter: w}

			log := s.log()
			level := slog.LevelInfo
			start := time.Now()
			requestID := fmt.Sprintf("%d", time.Now().UnixNano())

			defer func() {
				p := recover()
				if p != nil {
					log = log.With("panic", p, "request_id", requestID)
					level = slog.LevelError

					// Handle panic with user-friendly error
					if !sw.Written() {
						s.handleError(sw, fmt.Errorf("internal server error"))
					}
				}

				log.Log(r.Context(), level, "site.serveHTTP",
					"http.method", r.Method,
					"http.path", r.URL.Path,
					"http.pattern", r.Pattern,
					"http.status", sw.Status(),
					"http.d", time.Since(start),
					"request_id", requestID,
					"version", version.Version,
				)

				// let net/http.Server deal with panics
				if p != nil {
					panic(p)
				}
			}()

			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("X-Version", version.Version)
			w.Header().Set("X-Request-ID", requestID)

			ctx := r.Context()
			if err := f(sw, r); err != nil {
				if ctx.Err() != nil {
					return
				}
				level = slog.LevelError
				log = log.With("error", err)
				s.handleError(sw, err)
			}
		})
	}

	mux := http.NewServeMux()

	// CORS is handled in `handle`, but we have to match on OPTIONS to handle preflight requests
	mux.Handle("OPTIONS /", handle(func(w http.ResponseWriter, r *http.Request) error {
		return nil
	}))

	// API routes - handle first to take precedence
	mux.Handle("GET /api/v1/chats", handle(s.listChats))
	mux.Handle("GET /api/v1/chat/{id}", handle(s.getChat))
	mux.Handle("POST /api/v1/chat/{id}", handle(s.chat))
	mux.Handle("DELETE /api/v1/chat/{id}", handle(s.deleteChat))
	mux.Handle("POST /api/v1/create-chat", handle(s.createChat))
	mux.Handle("PUT /api/v1/chat/{id}/rename", handle(s.renameChat))

	mux.Handle("GET /api/v1/health", handle(s.getHealth))
	mux.Handle("GET /api/v1/inference-compute", handle(s.getInferenceCompute))
	mux.Handle("POST /api/v1/model/upstream", handle(s.modelUpstream))
	mux.Handle("GET /api/v1/settings", handle(s.getSettings))
	mux.Handle("POST /api/v1/settings", handle(s.settings))

	mux.Handle("GET /api/tags", handle(s.copilotTagsHandler))
	mux.Handle("POST /api/show", handle(s.copilotShowHandler))
	mux.Handle("GET /api/version", handle(s.apiVersion))
	mux.Handle("GET /api/status", handle(s.apiStatus))
	mux.Handle("HEAD /api/version", handle(s.apiVersion))
	mux.Handle("POST /api/me", handle(s.copilotUserHandler))
	mux.Handle("POST /api/signout", handle(s.copilotSignOutHandler))
	mux.Handle("GET /auth/github", handle(func(w http.ResponseWriter, r *http.Request) error {
		s.copilotAuthPageHandler(w, r)
		return nil
	}))
	mux.Handle("GET /auth/github/status", handle(s.copilotAuthStatusHandler))

	// React app - catch all non-API routes and serve the React app
	mux.Handle("GET /", s.appHandler())
	mux.Handle("PUT /", s.appHandler())
	mux.Handle("POST /", s.appHandler())
	mux.Handle("PATCH /", s.appHandler())
	mux.Handle("DELETE /", s.appHandler())

	return mux
}

// handleError renders appropriate error responses based on request type
func (s *Server) handleError(w http.ResponseWriter, e error) {
	// Preserve CORS headers for API requests
	if CORS() {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, User-Agent, Accept, X-Requested-With")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	json.NewEncoder(w).Encode(map[string]string{"error": e.Error()})
}

// userAgentTransport is a custom RoundTripper that adds the User-Agent header to all requests
type userAgentTransport struct {
	base http.RoundTripper
}

func (t *userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone the request to avoid mutating the original
	r := req.Clone(req.Context())
	r.Header.Set("User-Agent", userAgent())
	return t.base.RoundTrip(r)
}

// httpClient returns an HTTP client that automatically adds the User-Agent header
func (s *Server) httpClient() *http.Client {
	return userAgentHTTPClient(10 * time.Second)
}

// copilotChatHTTPClient disables the client-level timeout so streaming Copilot
// responses with larger prompts or attachments are not truncated prematurely.
func (s *Server) copilotChatHTTPClient() *http.Client {
	return userAgentHTTPClient(0)
}

// inferenceClient uses almost the same HTTP client, but without a timeout so
// long requests aren't truncated
func (s *Server) inferenceClient() *api.Client {
	return api.NewClient(envconfig.Host(), userAgentHTTPClient(0))
}

func userAgentHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &userAgentTransport{
			base: http.DefaultTransport,
		},
	}
}

// UserData fetches user data for the current desktop provider session.
func (s *Server) UserData(ctx context.Context) (*api.UserResponse, error) {
	session, err := s.Store.AuthSession()
	if err != nil {
		return nil, err
	}
	if session == nil || session.AccessToken == "" {
		return nil, nil
	}

	current, err := s.copilotAuthorizedSession(ctx)
	if err != nil {
		var authErr api.AuthorizationError
		if errors.As(err, &authErr) {
			return nil, nil
		}
		return nil, err
	}

	return &api.UserResponse{
		ID:        uuid.Nil,
		Email:     current.Email,
		Name:      current.Name,
		AvatarURL: current.AvatarURL,
		Plan:      current.Plan,
	}, nil
}

// WaitForServer waits for the Ollama server to be ready
func WaitForServer(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := api.ClientFromEnvironment()
		if err != nil {
			return err
		}
		if _, err := c.Version(ctx); err == nil {
			slog.Debug("ollama server is ready")
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("timeout waiting for Ollama server to be ready")
}

func (s *Server) createChat(w http.ResponseWriter, r *http.Request) error {
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("failed to generate chat ID: %w", err)
	}

	json.NewEncoder(w).Encode(map[string]string{"id": id.String()})
	return nil
}

func (s *Server) listChats(w http.ResponseWriter, r *http.Request) error {
	chatInfos, err := s.listChatInfos(r.Context())
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(responses.ChatsResponse{ChatInfos: chatInfos})
	return nil
}

// checkModelUpstream makes a HEAD request to the Ollama registry to get the upstream digest and push time
func (s *Server) checkModelUpstream(ctx context.Context, modelName string, timeout time.Duration) (string, int64, error) {
	// Create a context with timeout for the registry check
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Parse model name to get namespace, model, and tag
	parts := strings.Split(modelName, ":")
	name := parts[0]
	tag := "latest"
	if len(parts) > 1 {
		tag = parts[1]
	}

	if !strings.Contains(name, "/") {
		// If the model name does not contain a slash, assume it's a library model
		name = "library/" + name
	}

	// Check the model in the Ollama registry using HEAD request
	url := OllamaDotCom + "/v2/" + name + "/manifests/" + tag
	req, err := http.NewRequestWithContext(checkCtx, "HEAD", url, nil)
	if err != nil {
		return "", 0, err
	}

	httpClient := s.httpClient()
	httpClient.Timeout = timeout

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("registry returned status %d", resp.StatusCode)
	}

	digest := resp.Header.Get("ollama-content-digest")
	if digest == "" {
		return "", 0, fmt.Errorf("no digest header found")
	}

	var pushTime int64
	if pushTimeStr := resp.Header.Get("ollama-push-time"); pushTimeStr != "" {
		if pt, err := strconv.ParseInt(pushTimeStr, 10, 64); err == nil {
			pushTime = pt
		}
	}

	return digest, pushTime, nil
}

// isNetworkError checks if an error string contains common network/connection error patterns
func isNetworkError(errStr string) bool {
	networkErrorPatterns := []string{
		"connection refused",
		"no such host",
		"timeout",
		"network is unreachable",
		"connection reset",
		"connection timed out",
		"temporary failure",
		"dial tcp",
		"i/o timeout",
		"context deadline exceeded",
		"broken pipe",
	}

	for _, pattern := range networkErrorPatterns {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}
	return false
}

var ErrNetworkOffline = errors.New("network is offline")

func (s *Server) getError(err error) responses.ErrorEvent {
	var sErr api.AuthorizationError
	if errors.As(err, &sErr) && sErr.StatusCode == http.StatusUnauthorized {
		return responses.ErrorEvent{
			EventName: "error",
			Error:     "Could not verify you are signed in. Please sign in and try again.",
			Code:      "cloud_unauthorized",
		}
	}

	errStr := err.Error()

	switch {
	case strings.Contains(errStr, "402"):
		return responses.ErrorEvent{
			EventName: "error",
			Error:     "You've reached your usage limit, please upgrade to continue",
			Code:      "usage_limit_upgrade",
		}
	case strings.HasPrefix(errStr, "pull model manifest") && isNetworkError(errStr):
		return responses.ErrorEvent{
			EventName: "error",
			Error:     "Unable to download model. Please check your internet connection to download the model for offline use.",
			Code:      "offline_download_error",
		}
	case errors.Is(err, ErrNetworkOffline) || strings.Contains(errStr, "operation timed out"):
		return responses.ErrorEvent{
			EventName: "error",
			Error:     "Connection lost",
			Code:      "turbo_connection_lost",
		}
	}
	return responses.ErrorEvent{
		EventName: "error",
		Error:     err.Error(),
	}
}

func (s *Server) browserState(chat *store.Chat) (*responses.BrowserStateData, bool) {
	if len(chat.BrowserState) > 0 {
		var st responses.BrowserStateData
		if err := json.Unmarshal(chat.BrowserState, &st); err == nil {
			return &st, true
		}
	}
	return nil, false
}

// reconstructBrowserState (legacy): return the latest full browser state stored in messages.
func reconstructBrowserState(messages []store.Message, defaultViewTokens int) *responses.BrowserStateData {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.ToolResult == nil {
			continue
		}
		var st responses.BrowserStateData
		if err := json.Unmarshal(*msg.ToolResult, &st); err == nil {
			if len(st.PageStack) > 0 || len(st.URLToPage) > 0 {
				if st.ViewTokens == 0 {
					st.ViewTokens = defaultViewTokens
				}
				return &st
			}
		}
	}
	return nil
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "text/jsonl")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Transfer-Encoding", "chunked")

	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("streaming not supported")
	}

	if r.Method != "POST" {
		return not.Found
	}

	cid := r.PathValue("id")
	createdChat := false

	// if cid is the literal string "new", then we create a new chat before
	// performing our normal actions
	if cid == "new" {
		u, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("failed to generate new chat id: %w", err)
		}
		cid = u.String()
		createdChat = true
	}

	var req responses.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fmt.Fprintf(os.Stderr, "error unmarshalling body: %v\n", err)
		return fmt.Errorf("invalid request body: %w", err)
	}

	if req.Model == "" {
		return fmt.Errorf("empty model")
	}

	// Don't allow empty messages unless forceUpdate is true
	if req.Prompt == "" && !req.ForceUpdate {
		return fmt.Errorf("empty message")
	}

	session, err := s.copilotAuthorizedSession(r.Context())
	if err != nil {
		errorEvent := s.getError(err)
		json.NewEncoder(w).Encode(errorEvent)
		flusher.Flush()
		return nil
	}

	if createdChat {
		// send message to the client that the chat has been created
		json.NewEncoder(w).Encode(responses.ChatEvent{
			EventName: "chat_created",
			ChatID:    &cid,
		})
		flusher.Flush()
	}

	// Check if this is from a specific message index (e.g. for editing)
	idx := -1
	if req.Index != nil {
		idx = *req.Index
	}

	chat, err := s.loadChatFromGitHub(r.Context(), session, cid)
	if err != nil {
		if !errors.Is(err, not.Found) {
			return err
		}
		chat = store.NewChat(cid)
	}

	// Only add user message if not forceUpdate
	if !req.ForceUpdate {
		var messageOptions *store.MessageOptions
		if len(req.Attachments) > 0 {
			storeAttachments := make([]store.File, 0, len(req.Attachments))

			for _, att := range req.Attachments {
				if att.Data == "" {
					// This is an existing file reference - keep it from the original message
					if idx >= 0 && idx < len(chat.Messages) {
						originalMessage := chat.Messages[idx]
						// Find the file by filename in the original message
						for _, originalFile := range originalMessage.Attachments {
							if originalFile.Filename == att.Filename {
								storeAttachments = append(storeAttachments, originalFile)
								break
							}
						}
					}
				} else {
					// This is a new file - decode base64 data
					data, err := base64.StdEncoding.DecodeString(att.Data)
					if err != nil {
						s.log().Error("failed to decode attachment data", "error", err, "filename", att.Filename)
						continue
					}

					storeAttachments = append(storeAttachments, store.File{
						Filename: att.Filename,
						Data:     data,
					})
				}
			}

			messageOptions = &store.MessageOptions{
				Attachments: storeAttachments,
			}
		}
		userMsg := store.NewMessage("user", req.Prompt, messageOptions)

		if idx >= 0 && idx < len(chat.Messages) {
			// Generate from specified message: truncate and replace
			chat.Messages = chat.Messages[:idx]
			chat.Messages = append(chat.Messages, userMsg)
		} else {
			// Normal mode: append new message
			chat.Messages = append(chat.Messages, userMsg)
		}

		if err := s.storeChatRemotely(session, *chat); err != nil {
			return err
		}
	}

	remoteModel, err := s.copilotFindModel(r.Context(), req.Model)
	if err != nil {
		var authErr api.AuthorizationError
		if errors.As(err, &authErr) {
			errorEvent := s.getError(err)
			json.NewEncoder(w).Encode(errorEvent)
			flusher.Flush()
			return nil
		}
		s.log().Error("failed to resolve remote model", "model", req.Model, "error", err)
		errorEvent := s.getError(err)
		json.NewEncoder(w).Encode(errorEvent)
		flusher.Flush()
		return nil
	}
	if remoteModel != nil {
		return s.chatCopilot(r.Context(), w, flusher, session, chat, req, remoteModel)
	}
	errorEvent := s.getError(fmt.Errorf("model %q is not available in GitHub Copilot", req.Model))
	json.NewEncoder(w).Encode(errorEvent)
	flusher.Flush()
	return nil
}

func (s *Server) getChat(w http.ResponseWriter, r *http.Request) error {
	cid := r.PathValue("id")

	if cid == "" {
		return fmt.Errorf("chat ID is required")
	}

	session, err := s.githubChatAuthorizedSession(r.Context())
	if err != nil {
		if isAuthorizationError(err) {
			data := responses.ChatResponse{Chat: store.Chat{}}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(data)
			return nil
		}
		return err
	}

	chat, err := s.loadChatFromGitHub(r.Context(), session, cid)
	if err != nil {
		if errors.Is(err, not.Found) {
			data := responses.ChatResponse{
				Chat: store.Chat{},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(data)
			return nil //nolint:nilerr
		}
		return err
	}

	// fill missing tool_name on tool messages (from previous tool_calls) so labels don’t flip after reload.
	if chat != nil && len(chat.Messages) > 0 {
		for i := range chat.Messages {
			if chat.Messages[i].Role == "tool" && chat.Messages[i].ToolName == "" && chat.Messages[i].ToolResult != nil {
				for j := i - 1; j >= 0; j-- {
					if chat.Messages[j].Role == "assistant" && len(chat.Messages[j].ToolCalls) > 0 {
						last := chat.Messages[j].ToolCalls[len(chat.Messages[j].ToolCalls)-1]
						if last.Function.Name != "" {
							chat.Messages[i].ToolName = last.Function.Name
						}
						break
					}
				}
			}
		}
	}

	browserState, ok := s.browserState(chat)
	if !ok {
		browserState = reconstructBrowserState(chat.Messages, tools.DefaultViewTokens)
	}
	// clear the text and lines of all pages as it is not needed for rendering
	if browserState != nil {
		for _, page := range browserState.URLToPage {
			page.Lines = nil
			page.Text = ""
		}

		if cleanedState, err := json.Marshal(browserState); err == nil {
			chat.BrowserState = json.RawMessage(cleanedState)
		}
	}
	data := responses.ChatResponse{
		Chat: *chat,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
	return nil
}

func (s *Server) renameChat(w http.ResponseWriter, r *http.Request) error {
	cid := r.PathValue("id")
	if cid == "" {
		return fmt.Errorf("chat ID is required")
	}

	var req struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}

	session, err := s.githubChatAuthorizedSession(r.Context())
	if err != nil {
		return err
	}

	chat, err := s.loadChatFromGitHub(r.Context(), session, cid)
	if err != nil {
		return fmt.Errorf("chat not found: %w", err)
	}

	// Update the title
	chat.Title = req.Title
	if err := s.storeChatRemotely(session, *chat); err != nil {
		return fmt.Errorf("failed to update chat: %w", err)
	}

	// Return the updated chat info
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(chatInfoFromChat(*chat))
	return nil
}

func (s *Server) deleteChat(w http.ResponseWriter, r *http.Request) error {
	cid := r.PathValue("id")
	if cid == "" {
		return fmt.Errorf("chat ID is required")
	}

	session, err := s.githubChatAuthorizedSession(r.Context())
	if err != nil {
		return err
	}

	if err := s.migrateLocalChatToGitHub(r.Context(), session, cid); err != nil {
		return fmt.Errorf("failed to prepare chat: %w", err)
	}

	_, found, err := s.loadGitHubChat(r.Context(), session, cid)
	if err != nil {
		return fmt.Errorf("failed to get chat: %w", err)
	}
	if !found {
		if err := s.deleteLocalChatCache(cid); err != nil {
			s.log().Warn("failed to remove local chat cache", "chat_id", cid, "error", err)
		}
		w.WriteHeader(http.StatusNotFound)
		return fmt.Errorf("chat not found")
	}

	if err := s.deleteChatRemotely(session, cid); err != nil {
		return fmt.Errorf("failed to delete chat: %w", err)
	}

	w.WriteHeader(http.StatusOK)
	return nil
}

func chatInfoFromChat(chat store.Chat) responses.ChatInfo {
	userExcerpt := ""
	var updatedAt time.Time

	for _, msg := range chat.Messages {
		// extract the first user message as the user excerpt
		if msg.Role == "user" && userExcerpt == "" {
			userExcerpt = msg.Content
		}
		// update the updated at time
		if msg.UpdatedAt.After(updatedAt) {
			updatedAt = msg.UpdatedAt
		}
	}

	return responses.ChatInfo{
		ID:          chat.ID,
		Title:       chat.Title,
		UserExcerpt: userExcerpt,
		CreatedAt:   chat.CreatedAt,
		UpdatedAt:   updatedAt,
	}
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) error {
	settings, err := s.Store.Settings()
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	// set default models directory if not set
	if settings.Models == "" {
		settings.Models = envconfig.Models()
	}

	// Include current runtime settings
	settings.Agent = s.Agent
	settings.Tools = s.Tools
	settings.WorkingDir = s.WorkingDir

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(responses.SettingsResponse{
		Settings: settings,
	})
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) error {
	old, err := s.Store.Settings()
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	var settings store.Settings
	if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}

	if err := s.Store.SetSettings(settings); err != nil {
		return fmt.Errorf("failed to save settings: %w", err)
	}

	if old.ContextLength != settings.ContextLength ||
		old.Models != settings.Models ||
		old.Expose != settings.Expose {
		s.Restart()
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(responses.SettingsResponse{
		Settings: settings,
	})
}

func (s *Server) getHealth(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{
		"ok": true,
	})
}

func (s *Server) getInferenceCompute(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(responses.InferenceComputeResponse{})
}

func (s *Server) modelUpstream(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(responses.ModelUpstreamResponse{
		Stale: false,
		Error: "local model updates are not available in the Windows Copilot desktop app",
	})
}

func userAgent() string {
	buildinfo, _ := debug.ReadBuildInfo()

	version := buildinfo.Main.Version
	if version == "(devel)" {
		// When using `go run .` the version is "(devel)". This is seen
		// as an invalid version by ollama.com and so it defaults to
		// "needs upgrade" for some requests, such as pulls. These
		// checks can be skipped by using the special version "v0.0.0",
		// so we set it to that here.
		version = "v0.0.0"
	}

	return fmt.Sprintf("ollama/%s (%s %s) app/%s Go/%s",
		version,
		runtime.GOARCH,
		runtime.GOOS,
		version,
		runtime.Version(),
	)
}

// convertToOllamaTool converts a tool schema from our tools package format to Ollama API format
func convertToOllamaTool(toolSchema map[string]any) api.Tool {
	tool := api.Tool{
		Type: "function",
		Function: api.ToolFunction{
			Name:        getStringFromMap(toolSchema, "name", ""),
			Description: getStringFromMap(toolSchema, "description", ""),
		},
	}

	tool.Function.Parameters.Type = "object"
	tool.Function.Parameters.Required = []string{}
	tool.Function.Parameters.Properties = api.NewToolPropertiesMap()

	if schemaProps, ok := toolSchema["schema"].(map[string]any); ok {
		tool.Function.Parameters.Type = getStringFromMap(schemaProps, "type", "object")

		if props, ok := schemaProps["properties"].(map[string]any); ok {
			tool.Function.Parameters.Properties = api.NewToolPropertiesMap()

			for propName, propDef := range props {
				if propMap, ok := propDef.(map[string]any); ok {
					prop := api.ToolProperty{
						Type:        api.PropertyType{getStringFromMap(propMap, "type", "string")},
						Description: getStringFromMap(propMap, "description", ""),
					}
					tool.Function.Parameters.Properties.Set(propName, prop)
				}
			}
		}

		if required, ok := schemaProps["required"].([]string); ok {
			tool.Function.Parameters.Required = required
		} else if requiredAny, ok := schemaProps["required"].([]any); ok {
			required := make([]string, len(requiredAny))
			for i, r := range requiredAny {
				if s, ok := r.(string); ok {
					required[i] = s
				}
			}
			tool.Function.Parameters.Required = required
		}
	}

	return tool
}

// getStringFromMap safely gets a string from a map
func getStringFromMap(m map[string]any, key, defaultValue string) string {
	if val, ok := m[key].(string); ok {
		return val
	}
	return defaultValue
}

// isImageAttachment checks if a filename is an image file
func isImageAttachment(filename string) bool {
	ext := strings.ToLower(filename)
	return strings.HasSuffix(ext, ".png") || strings.HasSuffix(ext, ".jpg") || strings.HasSuffix(ext, ".jpeg") || strings.HasSuffix(ext, ".webp")
}

// ptr is a convenience function for &literal
func ptr[T any](v T) *T { return &v }

// Browser tools simulate a full browser environment, allowing for actions like searching, opening, and interacting with web pages (e.g., "browser_search", "browser_open", "browser_find"). Currently only gpt-oss models support browser tools.
func supportsBrowserTools(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "gpt-oss")
}

// buildChatRequest converts store.Chat to api.ChatRequest
func (s *Server) buildChatRequest(chat *store.Chat, model string, think any, availableTools []map[string]any) (*api.ChatRequest, error) {
	var msgs []api.Message
	for _, m := range chat.Messages {
		// Skip empty messages if present
		if m.Content == "" && m.Thinking == "" && len(m.ToolCalls) == 0 && len(m.Attachments) == 0 {
			continue
		}

		apiMsg := api.Message{Role: m.Role, Thinking: m.Thinking}

		sb := strings.Builder{}
		sb.WriteString(m.Content)

		var images []api.ImageData
		if m.Role == "user" && len(m.Attachments) > 0 {
			for _, a := range m.Attachments {
				if isImageAttachment(a.Filename) {
					images = append(images, api.ImageData(a.Data))
				} else {
					content := convertBytesToText(a.Data, a.Filename)
					sb.WriteString(fmt.Sprintf("\n--- File: %s ---\n%s\n--- End of %s ---",
						a.Filename, content, a.Filename))
				}
			}
		}

		apiMsg.Content = sb.String()
		apiMsg.Images = images

		switch m.Role {
		case "assistant":
			if len(m.ToolCalls) > 0 {
				var toolCalls []api.ToolCall
				for _, tc := range m.ToolCalls {
					var args api.ToolCallFunctionArguments
					if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
						s.log().Error("failed to parse tool call arguments", "error", err, "function_name", tc.Function.Name, "arguments", tc.Function.Arguments)
						continue
					}

					toolCalls = append(toolCalls, api.ToolCall{
						Function: api.ToolCallFunction{
							Name:      tc.Function.Name,
							Arguments: args,
						},
					})
				}
				apiMsg.ToolCalls = toolCalls
			}
		case "tool":
			apiMsg.Role = "tool"
			apiMsg.Content = m.Content
			apiMsg.ToolName = m.ToolName
		case "user", "system":
			// User and system messages are handled normally
		default:
			// Log unknown roles but still include them
			s.log().Debug("unknown message role", "role", m.Role)
		}

		msgs = append(msgs, apiMsg)
	}

	var thinkValue *api.ThinkValue
	if think != nil {
		// Only set Think if it's actually requesting thinking
		if boolValue, ok := think.(bool); ok {
			if boolValue {
				thinkValue = &api.ThinkValue{Value: boolValue}
			}
		} else if stringValue, ok := think.(string); ok {
			if stringValue != "" && stringValue != "none" {
				thinkValue = &api.ThinkValue{Value: stringValue}
			}
		}
	}

	req := &api.ChatRequest{
		Model:    model,
		Messages: msgs,
		Stream:   ptr(true),
		Think:    thinkValue,
	}

	if len(availableTools) > 0 {
		tools := make(api.Tools, len(availableTools))
		for i, toolSchema := range availableTools {
			tools[i] = convertToOllamaTool(toolSchema)
		}
		req.Tools = tools
	}

	return req, nil
}
