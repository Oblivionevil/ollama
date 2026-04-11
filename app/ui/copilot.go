//go:build windows || darwin || android

package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/app/store"
	"github.com/ollama/ollama/app/ui/responses"
	"github.com/ollama/ollama/types/model"
)

const (
	defaultGitHubOAuthClientID  = "01ab8ac9400c4e429b23"
	defaultCopilotPluginVersion = "copilot-chat/0.42.3"
	defaultCopilotEditorVersion = "vscode/1.101.0"
	defaultCopilotIntegrationID = "code-oss"

	githubAPIVersion    = "2022-11-28"
	copilotAPIVersion   = "2025-10-01"
	copilotAuthProvider = "github-copilot"

	copilotModelCacheTTL = 5 * time.Minute
)

type copilotDeviceFlow struct {
	ID              string
	DeviceCode      string
	UserCode        string
	VerificationURI string
	ExpiresAt       time.Time
	Interval        time.Duration
	NextPollAt      time.Time
	Status          string
	Error           string
}

type copilotDeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

type copilotDeviceTokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

type copilotTokenEnvelope struct {
	Token                string            `json:"token"`
	ExpiresAt            int64             `json:"expires_at"`
	RefreshIn            int64             `json:"refresh_in"`
	SKU                  string            `json:"sku"`
	Individual           bool              `json:"individual"`
	CodeReviewEnabled    bool              `json:"code_review_enabled"`
	CopilotIgnoreEnabled bool              `json:"copilotignore_enabled"`
	PublicSuggestions    string            `json:"public_suggestions"`
	Telemetry            string            `json:"telemetry"`
	OrganizationList     []string          `json:"organization_list"`
	Endpoints            map[string]string `json:"endpoints"`
}

type copilotUserInfo struct {
	Username              string   `json:"username"`
	CopilotPlan           string   `json:"copilot_plan"`
	QuotaResetDate        any      `json:"quota_reset_date"`
	OrganizationLoginList []string `json:"organization_login_list"`
}

type githubUserResponse struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

type githubUserEmail struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

type copilotModelsEnvelope struct {
	Data []copilotModelMetadata `json:"data"`
}

type copilotModelMetadata struct {
	ID                   string            `json:"id"`
	Name                 string            `json:"name"`
	Vendor               string            `json:"vendor"`
	Version              string            `json:"version"`
	ModelPickerEnabled   bool              `json:"model_picker_enabled"`
	IsChatDefault        bool              `json:"is_chat_default"`
	IsChatFallback       bool              `json:"is_chat_fallback"`
	SupportedEndpoints   []string          `json:"supported_endpoints"`
	RequestHeaders       map[string]string `json:"requestHeaders"`
	LegacyRequestHeaders map[string]string `json:"request_headers"`
	Capabilities         struct {
		Type      string `json:"type"`
		Family    string `json:"family"`
		Tokenizer string `json:"tokenizer"`
		Limits    struct {
			MaxContextWindowTokens int `json:"max_context_window_tokens"`
			MaxPromptTokens        int `json:"max_prompt_tokens"`
			MaxOutputTokens        int `json:"max_output_tokens"`
			Vision                 struct {
				MaxPromptImages int `json:"max_prompt_images"`
			} `json:"vision"`
		} `json:"limits"`
		Supports struct {
			Streaming         bool `json:"streaming"`
			ToolCalls         bool `json:"tool_calls"`
			Vision            bool `json:"vision"`
			Prediction        bool `json:"prediction"`
			AdaptiveThinking  bool `json:"adaptive_thinking"`
			MinThinkingBudget int  `json:"min_thinking_budget"`
			MaxThinkingBudget int  `json:"max_thinking_budget"`
			ReasoningEffort   any  `json:"reasoning_effort"`
		} `json:"supports"`
	} `json:"capabilities"`
}

type copilotListModelResponse struct {
	Name            string           `json:"name"`
	Model           string           `json:"model"`
	Digest          string           `json:"digest,omitempty"`
	Remote          bool             `json:"remote,omitempty"`
	RemoteHost      string           `json:"remote_host,omitempty"`
	RemoteModel     string           `json:"remote_model,omitempty"`
	RequiresAuth    bool             `json:"requires_auth,omitempty"`
	ReasoningLevels []string         `json:"reasoning_levels,omitempty"`
	Details         api.ModelDetails `json:"details,omitempty"`
}

type copilotShowResponse struct {
	Capabilities    []model.Capability `json:"capabilities,omitempty"`
	ReasoningLevels []string           `json:"reasoning_levels,omitempty"`
	RequiresAuth    bool               `json:"requires_auth,omitempty"`
	RemoteHost      string             `json:"remote_host,omitempty"`
	RemoteModel     string             `json:"remote_model,omitempty"`
	Details         api.ModelDetails   `json:"details,omitempty"`
}

type copilotAuthStatusResponse struct {
	Status          string    `json:"status"`
	UserCode        string    `json:"user_code,omitempty"`
	VerificationURI string    `json:"verification_uri,omitempty"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
	Error           string    `json:"error,omitempty"`
}

var copilotAuthPageTemplate = template.Must(template.New("copilot-auth").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>GitHub Copilot Sign In</title>
  <style>
    :root {
      color-scheme: light dark;
      --bg: #0f172a;
      --panel: rgba(15, 23, 42, 0.88);
      --text: #e2e8f0;
      --muted: #94a3b8;
      --accent: #38bdf8;
      --accent-2: #22c55e;
      --border: rgba(148, 163, 184, 0.25);
    }
    body {
      margin: 0;
      min-height: 100vh;
      font-family: Segoe UI, system-ui, sans-serif;
      background: radial-gradient(circle at top, #1e293b, #020617 60%);
      color: var(--text);
      display: grid;
      place-items: center;
      padding: 24px;
    }
    main {
      width: min(680px, 100%);
      border: 1px solid var(--border);
      background: var(--panel);
      backdrop-filter: blur(20px);
      border-radius: 24px;
      padding: 32px;
      box-shadow: 0 24px 64px rgba(0, 0, 0, 0.35);
    }
    h1 {
      margin: 0 0 12px;
      font-size: clamp(28px, 5vw, 40px);
      line-height: 1.1;
    }
    p {
      margin: 0 0 16px;
      color: var(--muted);
      line-height: 1.6;
    }
    .code {
      margin: 24px 0;
      padding: 18px 20px;
      border-radius: 18px;
      border: 1px solid var(--border);
      background: rgba(15, 23, 42, 0.65);
      font-size: clamp(22px, 5vw, 34px);
      font-weight: 700;
      letter-spacing: 0.14em;
      text-align: center;
    }
    a.button {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      gap: 8px;
      padding: 14px 18px;
      border-radius: 14px;
      background: linear-gradient(135deg, var(--accent), var(--accent-2));
      color: #001018;
      text-decoration: none;
      font-weight: 700;
    }
    .status {
      margin-top: 20px;
      padding: 14px 16px;
      border-radius: 14px;
      border: 1px solid var(--border);
      background: rgba(15, 23, 42, 0.55);
      color: var(--muted);
    }
    .status.ok {
      color: var(--text);
      border-color: rgba(34, 197, 94, 0.35);
    }
    .status.error {
      color: #fecaca;
      border-color: rgba(248, 113, 113, 0.35);
    }
  </style>
</head>
<body>
  <main>
    <h1>Sign in to GitHub Copilot</h1>
    <p>Open GitHub in your browser, enter this one-time code, and then return to the app.</p>
    <div class="code">{{ .UserCode }}</div>
    <a class="button" href="{{ .VerificationURI }}" target="_blank" rel="noreferrer">Open GitHub Verification</a>
    <div class="status" id="status">Waiting for authorization...</div>
  </main>
  <script>
    const statusEl = document.getElementById("status");
    const statusUrl = "/auth/github/status?id={{ .FlowID }}";
    async function refreshStatus() {
      try {
        const response = await fetch(statusUrl, { cache: "no-store" });
        const data = await response.json();
        if (data.status === "complete") {
          statusEl.className = "status ok";
          statusEl.textContent = "Authorization completed. You can close this window and return to the app.";
          return true;
        }
        if (data.status === "error" || data.status === "expired") {
          statusEl.className = "status error";
          statusEl.textContent = data.error || "Authorization failed. Start the sign-in flow again from the app.";
          return true;
        }
        statusEl.className = "status";
        statusEl.textContent = "Waiting for authorization...";
      } catch (error) {
        statusEl.className = "status error";
        statusEl.textContent = "Unable to check authorization status.";
      }
      return false;
    }

    refreshStatus();
    const intervalId = setInterval(async () => {
      if (await refreshStatus()) {
        clearInterval(intervalId);
      }
    }, 3000);
  </script>
</body>
</html>`))

func (s *Server) copilotOAuthClientID() string {
	if value := os.Getenv("OLLAMA_COPILOT_GITHUB_CLIENT_ID"); value != "" {
		return value
	}
	if value := os.Getenv("GITHUB_OAUTH_CLIENT_ID"); value != "" {
		return value
	}
	return defaultGitHubOAuthClientID
}

func (s *Server) copilotPluginVersion() string {
	if value := os.Getenv("OLLAMA_COPILOT_PLUGIN_VERSION"); value != "" {
		return value
	}
	return defaultCopilotPluginVersion
}

func (s *Server) copilotEditorVersion() string {
	if value := os.Getenv("OLLAMA_COPILOT_EDITOR_VERSION"); value != "" {
		return value
	}
	return defaultCopilotEditorVersion
}

func (s *Server) copilotIntegrationID() string {
	if value := os.Getenv("OLLAMA_COPILOT_INTEGRATION_ID"); value != "" {
		return value
	}
	return defaultCopilotIntegrationID
}

func (s *Server) copilotEnsureState() {
	s.copilotStateMu.Lock()
	defer s.copilotStateMu.Unlock()

	if s.copilotSessionIDValue == "" {
		s.copilotSessionIDValue = uuid.NewString()
	}
	if s.copilotFlows == nil {
		s.copilotFlows = make(map[string]*copilotDeviceFlow)
	}
}

func (s *Server) copilotSessionID() string {
	s.copilotEnsureState()

	s.copilotStateMu.Lock()
	defer s.copilotStateMu.Unlock()
	return s.copilotSessionIDValue
}

func (s *Server) copilotMachineID() string {
	deviceID, err := s.Store.ID()
	if err == nil && deviceID != "" {
		return deviceID
	}
	return uuid.NewString()
}

func (s *Server) copilotLocalSignInURL(r *http.Request, flowID string) string {
	scheme := "http"
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme = forwarded
	} else if r.TLS != nil {
		scheme = "https"
	}

	endpoint := url.URL{
		Scheme: scheme,
		Host:   r.Host,
		Path:   "/auth/github",
	}
	query := endpoint.Query()
	query.Set("id", flowID)
	if s.Token != "" {
		query.Set("token", s.Token)
	}
	endpoint.RawQuery = query.Encode()
	return endpoint.String()
}

func (s *Server) copilotFlowByID(id string) (*copilotDeviceFlow, bool) {
	s.copilotEnsureState()

	s.copilotStateMu.Lock()
	defer s.copilotStateMu.Unlock()

	for key, flow := range s.copilotFlows {
		if time.Now().After(flow.ExpiresAt) && flow.Status == "pending" {
			flow.Status = "expired"
		}
		if flow.Status == "expired" && time.Since(flow.ExpiresAt) > time.Hour {
			delete(s.copilotFlows, key)
		}
	}

	flow, ok := s.copilotFlows[id]
	return flow, ok
}

func (s *Server) copilotSetFlow(flow *copilotDeviceFlow) {
	s.copilotEnsureState()

	s.copilotStateMu.Lock()
	defer s.copilotStateMu.Unlock()
	s.copilotFlows[flow.ID] = flow
}

func (s *Server) copilotClearFlows() {
	s.copilotEnsureState()

	s.copilotStateMu.Lock()
	defer s.copilotStateMu.Unlock()
	clear(s.copilotFlows)
}

func (s *Server) copilotCachedModels() []copilotModelMetadata {
	s.copilotStateMu.Lock()
	defer s.copilotStateMu.Unlock()

	if len(s.copilotModelCache) == 0 || time.Since(s.copilotModelCacheAt) > copilotModelCacheTTL {
		return nil
	}

	models := make([]copilotModelMetadata, len(s.copilotModelCache))
	copy(models, s.copilotModelCache)
	return models
}

func (s *Server) copilotStoreModelCache(models []copilotModelMetadata) {
	s.copilotStateMu.Lock()
	defer s.copilotStateMu.Unlock()

	s.copilotModelCache = make([]copilotModelMetadata, len(models))
	copy(s.copilotModelCache, models)
	s.copilotModelCacheAt = time.Now()
}

func (s *Server) copilotClearModelCache() {
	s.copilotStateMu.Lock()
	defer s.copilotStateMu.Unlock()

	s.copilotModelCache = nil
	s.copilotModelCacheAt = time.Time{}
}

func (s *Server) copilotClearAuthSession() {
	if err := s.Store.ClearAuthSession(); err != nil {
		s.log().Warn("failed to clear auth session", "error", err)
	}
	if err := s.Store.ClearUser(); err != nil {
		s.log().Warn("failed to clear cached user", "error", err)
	}
	s.copilotClearModelCache()
}

func (s *Server) copilotRequestHeaders(token, intent, requestID string, extra map[string]string) map[string]string {
	machineID := s.copilotMachineID()
	sessionID := s.copilotSessionID()
	pluginVersion := s.copilotPluginVersion()
	editorVersion := s.copilotEditorVersion()
	integrationID := s.copilotIntegrationID()

	headers := map[string]string{
		"Authorization":          "Bearer " + token,
		"X-GitHub-Api-Version":   copilotAPIVersion,
		"X-Request-Id":           requestID,
		"OpenAI-Intent":          intent,
		"X-Interaction-Type":     intent,
		"X-Agent-Task-Id":        requestID,
		"VScode-SessionId":       sessionID,
		"VScode-MachineId":       machineID,
		"Editor-Device-Id":       machineID,
		"Editor-Plugin-Version":  pluginVersion,
		"Editor-Version":         editorVersion,
		"Copilot-Integration-Id": integrationID,
		"User-Agent":             userAgent(),
	}

	for key, value := range extra {
		headers[key] = value
	}

	return headers
}

func (s *Server) doJSONRequest(ctx context.Context, method, endpoint string, headers map[string]string, body any) (*http.Response, []byte, error) {
	var requestBody io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal request body: %w", err)
		}
		requestBody = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, requestBody)
	if err != nil {
		return nil, nil, fmt.Errorf("create request: %w", err)
	}

	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}

	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, nil, err
	}

	data, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return resp, nil, fmt.Errorf("read response body: %w", readErr)
	}

	return resp, data, nil
}

func (s *Server) fetchCopilotTokenEnvelope(ctx context.Context, accessToken string) (*copilotTokenEnvelope, error) {
	headers := map[string]string{
		"Authorization":        "token " + accessToken,
		"X-GitHub-Api-Version": githubAPIVersion,
		"Accept":               "application/json",
		"User-Agent":           userAgent(),
	}

	resp, data, err := s.doJSONRequest(ctx, http.MethodGet, "https://api.github.com/copilot_internal/v2/token", headers, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch copilot token: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, api.AuthorizationError{StatusCode: http.StatusUnauthorized, Status: resp.Status}
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("fetch copilot token failed: %s", strings.TrimSpace(string(data)))
	}

	var envelope copilotTokenEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("parse copilot token: %w", err)
	}

	return &envelope, nil
}

func (s *Server) fetchCopilotUserInfo(ctx context.Context, accessToken string) (*copilotUserInfo, error) {
	headers := map[string]string{
		"Authorization":        "token " + accessToken,
		"X-GitHub-Api-Version": githubAPIVersion,
		"Accept":               "application/json",
		"User-Agent":           userAgent(),
	}

	resp, data, err := s.doJSONRequest(ctx, http.MethodGet, "https://api.github.com/copilot_internal/user", headers, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch copilot user info: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, api.AuthorizationError{StatusCode: http.StatusUnauthorized, Status: resp.Status}
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("fetch copilot user info failed: %s", strings.TrimSpace(string(data)))
	}

	var info copilotUserInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("parse copilot user info: %w", err)
	}

	return &info, nil
}

func (s *Server) fetchGitHubUser(ctx context.Context, accessToken string) (*githubUserResponse, error) {
	headers := map[string]string{
		"Authorization":        "token " + accessToken,
		"X-GitHub-Api-Version": githubAPIVersion,
		"Accept":               "application/vnd.github+json",
		"User-Agent":           userAgent(),
	}

	resp, data, err := s.doJSONRequest(ctx, http.MethodGet, "https://api.github.com/user", headers, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch github user: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, api.AuthorizationError{StatusCode: http.StatusUnauthorized, Status: resp.Status}
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("fetch github user failed: %s", strings.TrimSpace(string(data)))
	}

	var user githubUserResponse
	if err := json.Unmarshal(data, &user); err != nil {
		return nil, fmt.Errorf("parse github user: %w", err)
	}

	return &user, nil
}

func (s *Server) fetchGitHubPrimaryEmail(ctx context.Context, accessToken string) (string, error) {
	headers := map[string]string{
		"Authorization":        "token " + accessToken,
		"X-GitHub-Api-Version": githubAPIVersion,
		"Accept":               "application/vnd.github+json",
		"User-Agent":           userAgent(),
	}

	resp, data, err := s.doJSONRequest(ctx, http.MethodGet, "https://api.github.com/user/emails", headers, nil)
	if err != nil {
		return "", fmt.Errorf("fetch github emails: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return "", api.AuthorizationError{StatusCode: http.StatusUnauthorized, Status: resp.Status}
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", nil
	}

	var emails []githubUserEmail
	if err := json.Unmarshal(data, &emails); err != nil {
		return "", nil
	}

	for _, email := range emails {
		if email.Primary && email.Verified {
			return email.Email, nil
		}
	}
	for _, email := range emails {
		if email.Verified {
			return email.Email, nil
		}
	}

	return "", nil
}

func (s *Server) copilotRefreshAuthSession(ctx context.Context, session *store.AuthSession) (*store.AuthSession, error) {
	envelope, err := s.fetchCopilotTokenEnvelope(ctx, session.AccessToken)
	if err != nil {
		var authErr api.AuthorizationError
		if errors.As(err, &authErr) {
			s.copilotClearAuthSession()
		}
		return nil, err
	}

	info, err := s.fetchCopilotUserInfo(ctx, session.AccessToken)
	if err != nil {
		var authErr api.AuthorizationError
		if errors.As(err, &authErr) {
			s.copilotClearAuthSession()
			return nil, err
		}
		s.log().Warn("failed to refresh copilot user info", "error", err)
	}

	refreshed := *session
	refreshed.Provider = copilotAuthProvider
	refreshed.CopilotToken = envelope.Token
	expiresAt := time.Now().Add(time.Duration(envelope.RefreshIn+60) * time.Second)
	refreshed.CopilotTokenExpiresAt = &expiresAt
	if info != nil && info.CopilotPlan != "" {
		refreshed.Plan = info.CopilotPlan
	}

	if err := s.Store.SetAuthSession(refreshed); err != nil {
		return nil, err
	}

	if err := s.Store.SetUser(store.User{Name: refreshed.Name, Email: refreshed.Email, Plan: refreshed.Plan}); err != nil {
		s.log().Warn("failed to update cached user", "error", err)
	}

	return &refreshed, nil
}

func (s *Server) copilotAuthorizedSession(ctx context.Context) (*store.AuthSession, error) {
	session, err := s.Store.AuthSession()
	if err != nil {
		return nil, err
	}

	if session == nil || session.AccessToken == "" {
		return nil, api.AuthorizationError{StatusCode: http.StatusUnauthorized, Status: http.StatusText(http.StatusUnauthorized)}
	}

	if session.CopilotToken != "" && session.CopilotTokenExpiresAt != nil && time.Until(*session.CopilotTokenExpiresAt) > time.Minute {
		return session, nil
	}

	return s.copilotRefreshAuthSession(ctx, session)
}

func (s *Server) copilotCreateDeviceFlow(ctx context.Context) (*copilotDeviceFlow, error) {
	values := url.Values{}
	values.Set("client_id", s.copilotOAuthClientID())
	values.Set("scope", "repo read:user user:email")

	headers := map[string]string{
		"Accept":     "application/json",
		"User-Agent": userAgent(),
	}

	resp, data, err := s.doJSONRequest(ctx, http.MethodPost, "https://github.com/login/device/code?"+values.Encode(), headers, nil)
	if err != nil {
		return nil, fmt.Errorf("start device flow: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("start device flow failed: %s", strings.TrimSpace(string(data)))
	}

	var payload copilotDeviceCodeResponse
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("parse device flow response: %w", err)
	}

	now := time.Now()
	flow := &copilotDeviceFlow{
		ID:              uuid.NewString(),
		DeviceCode:      payload.DeviceCode,
		UserCode:        payload.UserCode,
		VerificationURI: payload.VerificationURI,
		ExpiresAt:       now.Add(time.Duration(payload.ExpiresIn) * time.Second),
		Interval:        time.Duration(payload.Interval) * time.Second,
		NextPollAt:      now.Add(time.Duration(payload.Interval) * time.Second),
		Status:          "pending",
	}

	s.copilotSetFlow(flow)
	return flow, nil
}

func (s *Server) copilotFinalizeGitHubLogin(ctx context.Context, accessToken string) error {
	githubUser, err := s.fetchGitHubUser(ctx, accessToken)
	if err != nil {
		return err
	}

	if githubUser.Email == "" {
		if email, emailErr := s.fetchGitHubPrimaryEmail(ctx, accessToken); emailErr == nil && email != "" {
			githubUser.Email = email
		}
	}

	envelope, err := s.fetchCopilotTokenEnvelope(ctx, accessToken)
	if err != nil {
		return err
	}

	info, err := s.fetchCopilotUserInfo(ctx, accessToken)
	if err != nil {
		return err
	}

	plan := info.CopilotPlan
	if plan == "" {
		plan = envelope.SKU
	}
	if plan == "" {
		plan = "individual"
	}

	expiresAt := time.Now().Add(time.Duration(envelope.RefreshIn+60) * time.Second)
	session := store.AuthSession{
		Provider:              copilotAuthProvider,
		UserID:                fmt.Sprintf("%d", githubUser.ID),
		Username:              githubUser.Login,
		Name:                  firstNonEmpty(githubUser.Name, githubUser.Login),
		Email:                 githubUser.Email,
		AvatarURL:             githubUser.AvatarURL,
		Plan:                  plan,
		AccessToken:           accessToken,
		CopilotToken:          envelope.Token,
		CopilotTokenExpiresAt: &expiresAt,
	}

	if err := s.Store.SetAuthSession(session); err != nil {
		return err
	}

	if err := s.Store.SetUser(store.User{Name: session.Name, Email: session.Email, Plan: session.Plan}); err != nil {
		s.log().Warn("failed to cache user after sign-in", "error", err)
	}

	s.copilotClearModelCache()
	return nil
}

func (s *Server) copilotPollDeviceFlow(ctx context.Context, flow *copilotDeviceFlow) {
	if flow == nil || flow.Status != "pending" {
		return
	}
	if time.Now().After(flow.ExpiresAt) {
		flow.Status = "expired"
		flow.Error = "This sign-in code expired. Start the sign-in flow again from the app."
		return
	}
	if time.Now().Before(flow.NextPollAt) {
		return
	}

	values := url.Values{}
	values.Set("client_id", s.copilotOAuthClientID())
	values.Set("device_code", flow.DeviceCode)
	values.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")

	headers := map[string]string{
		"Accept":     "application/json",
		"User-Agent": userAgent(),
	}

	resp, data, err := s.doJSONRequest(ctx, http.MethodPost, "https://github.com/login/oauth/access_token?"+values.Encode(), headers, nil)
	flow.NextPollAt = time.Now().Add(flow.Interval)
	if err != nil {
		flow.Status = "error"
		flow.Error = err.Error()
		return
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		flow.Status = "error"
		flow.Error = strings.TrimSpace(string(data))
		return
	}

	var tokenResp copilotDeviceTokenResponse
	if err := json.Unmarshal(data, &tokenResp); err != nil {
		flow.Status = "error"
		flow.Error = fmt.Sprintf("invalid token response: %v", err)
		return
	}

	if tokenResp.AccessToken != "" {
		if err := s.copilotFinalizeGitHubLogin(ctx, tokenResp.AccessToken); err != nil {
			flow.Status = "error"
			flow.Error = err.Error()
			return
		}
		flow.Status = "complete"
		flow.Error = ""
		return
	}

	switch tokenResp.Error {
	case "authorization_pending", "":
		return
	case "slow_down":
		flow.Interval += 5 * time.Second
		flow.NextPollAt = time.Now().Add(flow.Interval)
	case "expired_token":
		flow.Status = "expired"
		flow.Error = "This sign-in code expired. Start the sign-in flow again from the app."
	default:
		flow.Status = "error"
		flow.Error = firstNonEmpty(tokenResp.ErrorDescription, tokenResp.Error)
	}
}

func (s *Server) copilotMaybeCompletePendingFlow(ctx context.Context) {
	s.copilotEnsureState()

	s.copilotStateMu.Lock()
	flows := make([]*copilotDeviceFlow, 0, len(s.copilotFlows))
	for _, flow := range s.copilotFlows {
		flows = append(flows, flow)
	}
	s.copilotStateMu.Unlock()

	for _, flow := range flows {
		if flow.Status == "pending" {
			s.copilotPollDeviceFlow(ctx, flow)
		}
	}
}

func (s *Server) copilotRequestModelHeaders(meta copilotModelMetadata) map[string]string {
	headers := make(map[string]string)
	for key, value := range meta.LegacyRequestHeaders {
		headers[key] = value
	}
	for key, value := range meta.RequestHeaders {
		headers[key] = value
	}
	return headers
}

func (s *Server) copilotFetchModels(ctx context.Context) ([]copilotModelMetadata, error) {
	if cached := s.copilotCachedModels(); len(cached) > 0 {
		return cached, nil
	}

	session, err := s.copilotAuthorizedSession(ctx)
	if err != nil {
		return nil, err
	}

	requestID := uuid.NewString()
	headers := s.copilotRequestHeaders(session.CopilotToken, "model-access", requestID, nil)

	resp, data, err := s.doJSONRequest(ctx, http.MethodGet, "https://api.githubcopilot.com/models", headers, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch copilot models: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		s.copilotClearAuthSession()
		return nil, api.AuthorizationError{StatusCode: http.StatusUnauthorized, Status: resp.Status}
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("fetch copilot models failed: %s", strings.TrimSpace(string(data)))
	}

	var payload copilotModelsEnvelope
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("parse copilot models: %w", err)
	}

	models := make([]copilotModelMetadata, 0, len(payload.Data))
	for _, modelMetadata := range payload.Data {
		if !modelMetadata.ModelPickerEnabled {
			continue
		}
		if modelMetadata.Capabilities.Type != "" && modelMetadata.Capabilities.Type != "chat" {
			continue
		}
		models = append(models, modelMetadata)
	}

	sort.SliceStable(models, func(i, j int) bool {
		if models[i].IsChatDefault != models[j].IsChatDefault {
			return models[i].IsChatDefault
		}
		return strings.ToLower(models[i].ID) < strings.ToLower(models[j].ID)
	})

	s.copilotStoreModelCache(models)
	return models, nil
}

func (s *Server) copilotFindModel(ctx context.Context, modelName string) (*copilotModelMetadata, error) {
	models, err := s.copilotFetchModels(ctx)
	if err != nil {
		cached := s.copilotCachedModels()
		for i := range cached {
			if cached[i].ID == modelName || cached[i].Name == modelName {
				return &cached[i], nil
			}
		}
		return nil, err
	}

	for i := range models {
		if models[i].ID == modelName || models[i].Name == modelName {
			return &models[i], nil
		}
	}

	return nil, nil
}

func (s *Server) copilotReasoningLevels(meta copilotModelMetadata) []string {
	levels := make([]string, 0, 3)
	switch value := meta.Capabilities.Supports.ReasoningEffort.(type) {
	case bool:
		if value {
			levels = append(levels, "low", "medium", "high")
		}
	case string:
		if value != "" {
			levels = append(levels, value)
		}
	case []string:
		levels = append(levels, value...)
	case []any:
		for _, item := range value {
			if level, ok := item.(string); ok && level != "" {
				levels = append(levels, level)
			}
		}
	}

	if len(levels) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(levels))
	unique := make([]string, 0, len(levels))
	for _, level := range levels {
		if _, ok := seen[level]; ok {
			continue
		}
		seen[level] = struct{}{}
		unique = append(unique, level)
	}
	return unique
}

func (s *Server) copilotModelCapabilities(meta copilotModelMetadata) []model.Capability {
	capabilities := make([]model.Capability, 0, 2)
	if meta.Capabilities.Supports.Vision {
		capabilities = append(capabilities, model.CapabilityVision)
	}
	if meta.Capabilities.Supports.AdaptiveThinking || len(s.copilotReasoningLevels(meta)) > 0 {
		capabilities = append(capabilities, model.CapabilityThinking)
	}
	return capabilities
}

func (s *Server) copilotModelDetails(meta copilotModelMetadata) api.ModelDetails {
	details := api.ModelDetails{}
	if meta.Capabilities.Family != "" {
		details.Family = meta.Capabilities.Family
		details.Families = []string{meta.Capabilities.Family}
	}
	return details
}

func (s *Server) copilotListResponseModel(meta copilotModelMetadata) copilotListModelResponse {
	digest := meta.Version
	if digest == "" {
		digest = meta.ID
	}

	return copilotListModelResponse{
		Name:            meta.ID,
		Model:           meta.ID,
		Digest:          digest,
		Remote:          true,
		RemoteHost:      "github.com",
		RemoteModel:     meta.ID,
		RequiresAuth:    true,
		ReasoningLevels: s.copilotReasoningLevels(meta),
		Details:         s.copilotModelDetails(meta),
	}
}

func (s *Server) copilotShowResponseForModel(meta copilotModelMetadata) copilotShowResponse {
	return copilotShowResponse{
		Capabilities:    s.copilotModelCapabilities(meta),
		ReasoningLevels: s.copilotReasoningLevels(meta),
		RequiresAuth:    true,
		RemoteHost:      "github.com",
		RemoteModel:     meta.ID,
		Details:         s.copilotModelDetails(meta),
	}
}

func (s *Server) copilotUserHandler(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Connect bool `json:"connect"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	s.copilotMaybeCompletePendingFlow(r.Context())

	session, err := s.Store.AuthSession()
	if err != nil {
		return err
	}

	if session == nil || session.AccessToken == "" {
		if !req.Connect {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			return json.NewEncoder(w).Encode(map[string]string{"error": "not authenticated"})
		}

		flow, err := s.copilotCreateDeviceFlow(r.Context())
		if err != nil {
			return err
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		return json.NewEncoder(w).Encode(map[string]string{
			"signin_url": s.copilotLocalSignInURL(r, flow.ID),
		})
	}

	current, err := s.copilotAuthorizedSession(r.Context())
	if err != nil {
		var authErr api.AuthorizationError
		if errors.As(err, &authErr) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			return json.NewEncoder(w).Encode(map[string]string{"error": "not authenticated"})
		}
		return err
	}

	user := responses.User{
		ID:        firstNonEmpty(current.UserID, current.Username),
		Email:     current.Email,
		Name:      firstNonEmpty(current.Name, current.Username),
		AvatarURL: current.AvatarURL,
		Plan:      current.Plan,
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(user)
}

func (s *Server) copilotSignOutHandler(w http.ResponseWriter, r *http.Request) error {
	s.copilotClearAuthSession()
	s.copilotClearFlows()

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (s *Server) copilotAuthPageHandler(w http.ResponseWriter, r *http.Request) {
	flowID := r.URL.Query().Get("id")
	flow, ok := s.copilotFlowByID(flowID)
	if !ok {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := copilotAuthPageTemplate.Execute(w, map[string]string{
		"FlowID":          flow.ID,
		"UserCode":        flow.UserCode,
		"VerificationURI": flow.VerificationURI,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) copilotAuthStatusHandler(w http.ResponseWriter, r *http.Request) error {
	flowID := r.URL.Query().Get("id")
	flow, ok := s.copilotFlowByID(flowID)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return json.NewEncoder(w).Encode(copilotAuthStatusResponse{Status: "unknown", Error: "flow not found"})
	}

	if flow.Status == "pending" {
		s.copilotPollDeviceFlow(r.Context(), flow)
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(copilotAuthStatusResponse{
		Status:          flow.Status,
		UserCode:        flow.UserCode,
		VerificationURI: flow.VerificationURI,
		ExpiresAt:       flow.ExpiresAt,
		Error:           flow.Error,
	})
}

func (s *Server) copilotTagsHandler(w http.ResponseWriter, r *http.Request) error {
	models, err := s.copilotFetchModels(r.Context())
	if err != nil {
		var authErr api.AuthorizationError
		if !errors.As(err, &authErr) {
			s.log().Warn("failed to fetch copilot models", "error", err)
		}
		models = s.copilotCachedModels()
	}

	responseModels := make([]copilotListModelResponse, 0, len(models))
	for _, modelMetadata := range models {
		responseModels = append(responseModels, s.copilotListResponseModel(modelMetadata))
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{"models": responseModels})
}

func (s *Server) copilotShowHandler(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Model string `json:"model"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}

	modelName := firstNonEmpty(req.Model, req.Name)
	if modelName == "" {
		return fmt.Errorf("model is required")
	}

	modelMetadata, err := s.copilotFindModel(r.Context(), modelName)
	if err != nil {
		var authErr api.AuthorizationError
		if errors.As(err, &authErr) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			return json.NewEncoder(w).Encode(map[string]string{"error": "not authenticated"})
		}
		return err
	}

	if modelMetadata == nil {
		w.WriteHeader(http.StatusNotFound)
		return json.NewEncoder(w).Encode(map[string]string{"error": "model not found"})
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(s.copilotShowResponseForModel(*modelMetadata))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
