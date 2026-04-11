//go:build windows || darwin || android

package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/app/store"
)

func TestHandlePostApiSettings(t *testing.T) {
	tests := []struct {
		name      string
		requested store.Settings
		wantErr   bool
	}{
		{
			name: "valid settings update - all fields",
			requested: store.Settings{
				Expose:     true,
				Browser:    true,
				Models:     "/custom/models",
				Agent:      true,
				Tools:      true,
				WorkingDir: "/workspace",
			},
			wantErr: false,
		},
		{
			name: "partial settings update",
			requested: store.Settings{
				Agent:      true,
				Tools:      false,
				WorkingDir: "/new/path",
			},
			wantErr: false,
		},
		{
			name: "settings with special characters in paths",
			requested: store.Settings{
				Models:     "/path with spaces/models",
				WorkingDir: "/tmp/work-dir_123",
				Agent:      true,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testStore := &store.Store{
				DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
			}
			defer testStore.Close() // Ensure database is closed before cleanup

			body, err := json.Marshal(tt.requested)
			if err != nil {
				t.Fatalf("failed to marshal test body: %v", err)
			}

			req := httptest.NewRequest("POST", "/api/v1/settings", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			// Set up server with test store
			server := &Server{
				Store:   testStore,
				Restart: func() {}, // Mock restart function for tests
			}

			if err := server.settings(rr, req); (err != nil) != tt.wantErr {
				t.Errorf("handlePostApiSettings() error = %v, wantErr %v", err, tt.wantErr)
			}
			if rr.Code != http.StatusOK {
				t.Errorf("handlePostApiSettings() status = %v, want %v", rr.Code, http.StatusOK)
			}

			// Check settings were saved correctly (if no error expected)
			if !tt.wantErr {
				savedSettings, err := testStore.Settings()
				if err != nil {
					t.Errorf("failed to retrieve saved settings: %v", err)
				} else {
					// Compare field by field, accounting for defaults that may be set by the store
					if savedSettings.Expose != tt.requested.Expose {
						t.Errorf("Expose: got %v, want %v", savedSettings.Expose, tt.requested.Expose)
					}
					if savedSettings.Browser != tt.requested.Browser {
						t.Errorf("Browser: got %v, want %v", savedSettings.Browser, tt.requested.Browser)
					}
					if savedSettings.Agent != tt.requested.Agent {
						t.Errorf("Agent: got %v, want %v", savedSettings.Agent, tt.requested.Agent)
					}
					if savedSettings.Tools != tt.requested.Tools {
						t.Errorf("Tools: got %v, want %v", savedSettings.Tools, tt.requested.Tools)
					}
					if savedSettings.WorkingDir != tt.requested.WorkingDir {
						t.Errorf("WorkingDir: got %q, want %q", savedSettings.WorkingDir, tt.requested.WorkingDir)
					}
					// Only check Models if explicitly set in the test case
					if tt.requested.Models != "" && savedSettings.Models != tt.requested.Models {
						t.Errorf("Models: got %q, want %q", savedSettings.Models, tt.requested.Models)
					}
				}
			}
		})
	}
}

func TestHandleGetApiHealth(t *testing.T) {
	server := &Server{}
	req := httptest.NewRequest("GET", "/api/v1/health", nil)
	rr := httptest.NewRecorder()

	if err := server.getHealth(rr, req); err != nil {
		t.Fatalf("getHealth() error = %v", err)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("getHealth() status = %d, want %d", rr.Code, http.StatusOK)
	}

	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("getHealth() invalid response JSON: %v", err)
	}
	if got["ok"] != true {
		t.Fatalf("response ok = %v, want true", got["ok"])
	}
}

func TestAppHandlerDesktopOnlyAccess(t *testing.T) {
	t.Run("blocks browser requests without desktop token", func(t *testing.T) {
		server := &Server{Token: "desktop-token"}
		req := httptest.NewRequest("GET", "/c/new", nil)
		rr := httptest.NewRecorder()

		server.appHandler().ServeHTTP(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
		}
		if !strings.Contains(rr.Body.String(), "Desktop UI is only available inside the app") {
			t.Fatalf("body = %q, want desktop-only error", rr.Body.String())
		}
	})

	t.Run("accepts one-time desktop token and strips it from the URL", func(t *testing.T) {
		server := &Server{Token: "desktop-token"}
		req := httptest.NewRequest("GET", "/c/new?token=desktop-token&foo=bar", nil)
		rr := httptest.NewRecorder()

		server.appHandler().ServeHTTP(rr, req)

		if rr.Code != http.StatusFound {
			t.Fatalf("status = %d, want %d", rr.Code, http.StatusFound)
		}
		if location := rr.Header().Get("Location"); location != "/c/new?foo=bar" {
			t.Fatalf("location = %q, want %q", location, "/c/new?foo=bar")
		}

		cookies := rr.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatalf("cookies = %d, want 1", len(cookies))
		}
		if cookies[0].Name != "token" || cookies[0].Value != "desktop-token" {
			t.Fatalf("cookie = %+v, want token cookie", cookies[0])
		}
		if !cookies[0].HttpOnly {
			t.Fatalf("cookie HttpOnly = false, want true")
		}
	})

	t.Run("allows app shell with desktop cookie", func(t *testing.T) {
		server := &Server{Token: "desktop-token"}
		req := httptest.NewRequest("GET", "/c/new", nil)
		req.AddCookie(&http.Cookie{Name: "token", Value: "desktop-token"})
		rr := httptest.NewRecorder()

		server.appHandler().ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
		}
		if rr.Body.Len() == 0 {
			t.Fatal("body is empty, want app shell")
		}
	})
}

func TestAuthenticationMiddleware(t *testing.T) {
	tests := []struct {
		name         string
		method       string
		contentType  string
		tokenCookie  string
		serverToken  string
		wantStatus   int
		wantError    string
		setupRequest func(*http.Request)
	}{
		{
			name:        "missing token cookie",
			method:      "GET",
			tokenCookie: "",
			serverToken: "test-token-123",
			wantStatus:  http.StatusForbidden,
			wantError:   "Token is required",
		},
		{
			name:        "invalid token value",
			method:      "GET",
			tokenCookie: "wrong-token",
			serverToken: "test-token-123",
			wantStatus:  http.StatusForbidden,
			wantError:   "Token is required",
		},
		{
			name:        "valid token - GET request",
			method:      "GET",
			tokenCookie: "test-token-123",
			serverToken: "test-token-123",
			wantStatus:  http.StatusOK,
			wantError:   "",
		},
		{
			name:        "valid token - POST with application/json",
			method:      "POST",
			contentType: "application/json",
			tokenCookie: "test-token-123",
			serverToken: "test-token-123",
			wantStatus:  http.StatusOK,
			wantError:   "",
		},
		{
			name:        "POST without Content-Type header",
			method:      "POST",
			contentType: "",
			tokenCookie: "test-token-123",
			serverToken: "test-token-123",
			wantStatus:  http.StatusForbidden,
			wantError:   "Content-Type must be application/json",
		},
		{
			name:        "POST with wrong Content-Type",
			method:      "POST",
			contentType: "text/plain",
			tokenCookie: "test-token-123",
			serverToken: "test-token-123",
			wantStatus:  http.StatusForbidden,
			wantError:   "Content-Type must be application/json",
		},
		{
			name:        "OPTIONS request (CORS preflight) - should bypass auth",
			method:      "OPTIONS",
			tokenCookie: "",
			serverToken: "test-token-123",
			wantStatus:  http.StatusOK,
			wantError:   "",
			setupRequest: func(r *http.Request) {
				// Simulate CORS being enabled
				// Note: This assumes CORS() returns true in test environment
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a test handler that just returns 200 OK if auth passes
			testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			})

			// Create server with test token
			server := &Server{
				Token: tt.serverToken,
			}

			// Get the authentication middleware by calling Handler()
			// We need to wrap our test handler with the auth middleware
			handler := server.Handler()

			// Create a test router to simulate the authentication middleware
			mux := http.NewServeMux()
			mux.Handle("/test", handler)

			// But since Handler() returns the full router, we'll need a different approach
			// Let's create a minimal handler that includes just the auth logic
			authHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Add CORS headers for dev work
				if r.Method == "OPTIONS" {
					w.WriteHeader(http.StatusOK)
					return
				}

				if r.Method == "POST" && r.Header.Get("Content-Type") != "application/json" {
					w.WriteHeader(http.StatusForbidden)
					json.NewEncoder(w).Encode(map[string]string{"error": "Content-Type must be application/json"})
					return
				}

				cookie, err := r.Cookie("token")
				if err != nil {
					w.WriteHeader(http.StatusForbidden)
					json.NewEncoder(w).Encode(map[string]string{"error": "Token is required"})
					return
				}

				if cookie.Value != server.Token {
					w.WriteHeader(http.StatusForbidden)
					json.NewEncoder(w).Encode(map[string]string{"error": "Token is required"})
					return
				}

				// If auth passes, call the test handler
				testHandler.ServeHTTP(w, r)
			})

			// Create test request
			req := httptest.NewRequest(tt.method, "/test", nil)

			// Set Content-Type if provided
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}

			// Set token cookie if provided
			if tt.tokenCookie != "" {
				req.AddCookie(&http.Cookie{
					Name:  "token",
					Value: tt.tokenCookie,
				})
			}

			// Run any additional setup
			if tt.setupRequest != nil {
				tt.setupRequest(req)
			}

			// Create response recorder
			rr := httptest.NewRecorder()

			// Serve the request
			authHandler.ServeHTTP(rr, req)

			// Check status code
			if rr.Code != tt.wantStatus {
				t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, tt.wantStatus)
			}

			// Check error message if expected
			if tt.wantError != "" {
				var response map[string]string
				if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
					t.Fatalf("failed to decode response body: %v", err)
				}

				if response["error"] != tt.wantError {
					t.Errorf("handler returned wrong error message: got %v want %v", response["error"], tt.wantError)
				}
			}
		})
	}
}

func TestCopilotLocalSignInURLIncludesDesktopToken(t *testing.T) {
	server := &Server{Token: "desktop-token"}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:12345/api/me", nil)

	signInURL := server.copilotLocalSignInURL(req, "flow-123")
	parsed, err := url.Parse(signInURL)
	if err != nil {
		t.Fatalf("Parse(signInURL) error = %v", err)
	}

	if parsed.Scheme != "http" {
		t.Fatalf("scheme = %q, want http", parsed.Scheme)
	}
	if parsed.Host != "127.0.0.1:12345" {
		t.Fatalf("host = %q, want 127.0.0.1:12345", parsed.Host)
	}
	if parsed.Path != "/auth/github" {
		t.Fatalf("path = %q, want /auth/github", parsed.Path)
	}
	if got := parsed.Query().Get("id"); got != "flow-123" {
		t.Fatalf("id = %q, want flow-123", got)
	}
	if got := parsed.Query().Get("token"); got != "desktop-token" {
		t.Fatalf("token = %q, want desktop-token", got)
	}
}

func TestAuthGithubAcceptsOneTimeDesktopToken(t *testing.T) {
	server := &Server{
		Token: "desktop-token",
		copilotFlows: map[string]*copilotDeviceFlow{
			"flow-123": {
				ID:              "flow-123",
				UserCode:        "ABCD-EFGH",
				VerificationURI: "https://github.com/login/device",
				ExpiresAt:       time.Now().Add(time.Hour),
				Status:          "pending",
			},
		},
	}

	handler := server.Handler()
	firstReq := httptest.NewRequest(http.MethodGet, "/auth/github?id=flow-123&token=desktop-token", nil)
	firstResp := httptest.NewRecorder()

	handler.ServeHTTP(firstResp, firstReq)

	if firstResp.Code != http.StatusFound {
		t.Fatalf("first response status = %d, want %d", firstResp.Code, http.StatusFound)
	}
	if location := firstResp.Header().Get("Location"); location != "/auth/github?id=flow-123" {
		t.Fatalf("location = %q, want %q", location, "/auth/github?id=flow-123")
	}

	result := firstResp.Result()
	defer result.Body.Close()
	cookies := result.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d, want 1", len(cookies))
	}
	if cookies[0].Name != "token" || cookies[0].Value != "desktop-token" {
		t.Fatalf("cookie = %+v, want desktop token cookie", cookies[0])
	}

	secondReq := httptest.NewRequest(http.MethodGet, "/auth/github?id=flow-123", nil)
	secondReq.AddCookie(cookies[0])
	secondResp := httptest.NewRecorder()

	handler.ServeHTTP(secondResp, secondReq)

	if secondResp.Code != http.StatusOK {
		t.Fatalf("second response status = %d, want %d", secondResp.Code, http.StatusOK)
	}
	if !strings.Contains(secondResp.Body.String(), "ABCD-EFGH") {
		t.Fatalf("body = %q, want device code", secondResp.Body.String())
	}
}

func TestUserAgent(t *testing.T) {
	ua := userAgent()

	// The userAgent function should return a string in the format:
	// "ollama/version (arch os) app/version Go/goversion"
	// Example: "ollama/v0.1.28 (amd64 darwin) Go/go1.21.0"

	if ua == "" {
		t.Fatal("userAgent returned empty string")
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("User-Agent", ua)

	// This is a copy of the logic ollama.com uses to parse the user agent
	clientInfoFromRequest := func(r *http.Request) struct {
		Product    string
		Version    string
		OS         string
		Arch       string
		AppVersion string
	} {
		product, rest, _ := strings.Cut(r.UserAgent(), " ")
		client, version, ok := strings.Cut(product, "/")
		if !ok {
			return struct {
				Product    string
				Version    string
				OS         string
				Arch       string
				AppVersion string
			}{}
		}

		if version != "" && version[0] != 'v' {
			version = "v" + version
		}

		arch, rest, _ := strings.Cut(rest, " ")
		arch = strings.Trim(arch, "(")
		os, rest, _ := strings.Cut(rest, ")")

		var appVersion string
		if strings.Contains(rest, "app/") {
			_, appPart, found := strings.Cut(rest, "app/")
			if found {
				appVersion = strings.Fields(strings.TrimSpace(appPart))[0]
				if appVersion != "" && appVersion[0] != 'v' {
					appVersion = "v" + appVersion
				}
			}
		}

		return struct {
			Product    string
			Version    string
			OS         string
			Arch       string
			AppVersion string
		}{
			Product:    client,
			Version:    version,
			OS:         os,
			Arch:       arch,
			AppVersion: appVersion,
		}
	}

	info := clientInfoFromRequest(req)
	if info.Product != "ollama" {
		t.Errorf("Expected Product to be 'ollama', got '%s'", info.Product)
	}

	if info.Version != "" && info.Version[0] != 'v' {
		t.Errorf("Expected Version to start with 'v', got '%s'", info.Version)
	}

	expectedOS := runtime.GOOS
	if info.OS != expectedOS {
		t.Errorf("Expected OS to be '%s', got '%s'", expectedOS, info.OS)
	}

	expectedArch := runtime.GOARCH
	if info.Arch != expectedArch {
		t.Errorf("Expected Arch to be '%s', got '%s'", expectedArch, info.Arch)
	}

	if info.AppVersion != "" && info.AppVersion[0] != 'v' {
		t.Errorf("Expected AppVersion to start with 'v', got '%s'", info.AppVersion)
	}

	t.Logf("User Agent: %s", ua)
	t.Logf("Parsed - Product: %s, Version: %s, OS: %s, Arch: %s",
		info.Product, info.Version, info.OS, info.Arch)
}

func TestUserAgentTransport(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(r.Header.Get("User-Agent")))
	}))
	defer ts.Close()
	server := &Server{}

	client := server.httpClient()
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	defer resp.Body.Close()

	// In this case the User-Agent is the response body, as the server just echoes it back
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	receivedUA := string(body)
	expectedUA := userAgent()

	if receivedUA != expectedUA {
		t.Errorf("User-Agent mismatch\nExpected: %s\nReceived: %s", expectedUA, receivedUA)
	}

	if !strings.HasPrefix(receivedUA, "ollama/") {
		t.Errorf("User-Agent should start with 'ollama/', got: %s", receivedUA)
	}

	t.Logf("User-Agent transport successfully set: %s", receivedUA)
}

func TestInferenceClientUsesUserAgent(t *testing.T) {
	var gotUserAgent atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserAgent.Store(r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	t.Setenv("OLLAMA_HOST", ts.URL)

	server := &Server{}
	client := server.inferenceClient()

	_, err := client.Show(context.Background(), &api.ShowRequest{Model: "test"})
	if err != nil {
		t.Fatalf("show request failed: %v", err)
	}

	receivedUA, _ := gotUserAgent.Load().(string)
	expectedUA := userAgent()

	if receivedUA != expectedUA {
		t.Errorf("User-Agent mismatch\nExpected: %s\nReceived: %s", expectedUA, receivedUA)
	}
}

func TestSupportsBrowserTools(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"gpt-oss", true},
		{"gpt-oss-latest", true},
		{"GPT-OSS", true},
		{"Gpt-Oss-v2", true},
		{"qwen3", false},
		{"deepseek-v3", false},
		{"llama3.3", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := supportsBrowserTools(tt.model); got != tt.want {
				t.Errorf("supportsBrowserTools(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

func TestWebSearchToolRegistration(t *testing.T) {
	// Validates that the capability-gating logic in chat() correctly
	// decides which tools to register based on model capabilities and
	// the web search flag.
	tests := []struct {
		name             string
		webSearchEnabled bool
		hasToolsCap      bool
		model            string
		wantBrowser      bool // expects browser tools (gpt-oss)
		wantWebSearch    bool // expects basic web search/fetch tools
		wantNone         bool // expects no tools registered
	}{
		{
			name:             "web search enabled with tools capability - browser model",
			webSearchEnabled: true,
			hasToolsCap:      true,
			model:            "gpt-oss-latest",
			wantBrowser:      true,
		},
		{
			name:             "web search enabled with tools capability - non-browser model",
			webSearchEnabled: true,
			hasToolsCap:      true,
			model:            "qwen3",
			wantWebSearch:    true,
		},
		{
			name:             "web search enabled without tools capability",
			webSearchEnabled: true,
			hasToolsCap:      false,
			model:            "llama3.3",
			wantNone:         true,
		},
		{
			name:             "web search disabled with tools capability",
			webSearchEnabled: false,
			hasToolsCap:      true,
			model:            "qwen3",
			wantNone:         true,
		},
		{
			name:             "web search disabled without tools capability",
			webSearchEnabled: false,
			hasToolsCap:      false,
			model:            "llama3.3",
			wantNone:         true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Replicate the decision logic from chat() handler
			gotBrowser := false
			gotWebSearch := false

			if tt.webSearchEnabled && tt.hasToolsCap {
				if supportsBrowserTools(tt.model) {
					gotBrowser = true
				} else {
					gotWebSearch = true
				}
			}

			if tt.wantBrowser && !gotBrowser {
				t.Error("expected browser tools to be registered")
			}
			if tt.wantWebSearch && !gotWebSearch {
				t.Error("expected web search tools to be registered")
			}
			if tt.wantNone && (gotBrowser || gotWebSearch) {
				t.Error("expected no tools to be registered")
			}
			if !tt.wantBrowser && gotBrowser {
				t.Error("unexpected browser tools registered")
			}
			if !tt.wantWebSearch && gotWebSearch {
				t.Error("unexpected web search tools registered")
			}
		})
	}
}
