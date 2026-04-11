//go:build windows || darwin

package ui

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ollama/ollama/app/store"
	"github.com/ollama/ollama/app/ui/responses"
)

func TestSyncChatToGitHubWritesChatAndManifest(t *testing.T) {
	fakeAPI := newFakeGitHubContentsAPI(t)
	server := &Server{
		githubAPIBaseURL: fakeAPI.server.URL,
		githubChatRepo:   defaultGitHubChatRepo,
	}
	session := &store.AuthSession{AccessToken: "gh-test-token"}

	chat := store.Chat{
		ID:        "chat-1",
		Title:     "First chat",
		CreatedAt: time.Unix(1710000000, 0).UTC(),
		Messages: []store.Message{
			store.NewMessage("user", "hello github", nil),
		},
	}

	if err := server.syncChatToGitHub(context.Background(), session, chat); err != nil {
		t.Fatalf("syncChatToGitHub() error = %v", err)
	}

	rawChat := fakeAPI.fileContents(t, server.githubChatFilePath(chat.ID))
	var storedChat store.Chat
	if err := json.Unmarshal(rawChat, &storedChat); err != nil {
		t.Fatalf("unmarshal stored chat: %v", err)
	}
	if storedChat.ID != chat.ID {
		t.Fatalf("stored chat id = %q, want %q", storedChat.ID, chat.ID)
	}

	rawManifest := fakeAPI.fileContents(t, server.githubChatManifestPath())
	var manifest githubChatManifest
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if len(manifest.Chats) != 1 {
		t.Fatalf("manifest chat count = %d, want 1", len(manifest.Chats))
	}
	if manifest.Chats[0].ID != chat.ID {
		t.Fatalf("manifest chat id = %q, want %q", manifest.Chats[0].ID, chat.ID)
	}
	if fakeAPI.lastAuthorization != "Bearer gh-test-token" {
		t.Fatalf("authorization = %q, want Bearer gh-test-token", fakeAPI.lastAuthorization)
	}
}

func TestListChatsIncludesGitHubManifestEntries(t *testing.T) {
	fakeAPI := newFakeGitHubContentsAPI(t)
	testStore := &store.Store{DBPath: filepath.Join(t.TempDir(), "db.sqlite")}
	defer testStore.Close()

	if err := testStore.SetAuthSession(store.AuthSession{AccessToken: "gh-test-token"}); err != nil {
		t.Fatalf("SetAuthSession() error = %v", err)
	}

	manifest := githubChatManifest{
		Chats: []responses.ChatInfo{{
			ID:          "remote-chat",
			Title:       "Remote chat",
			UserExcerpt: "from github",
			CreatedAt:   time.Unix(1710000100, 0).UTC(),
			UpdatedAt:   time.Unix(1710000200, 0).UTC(),
		}},
		SyncedAt: time.Now().UTC(),
	}
	fakeAPI.putJSON(t, "chats/index.json", manifest)

	server := &Server{
		Store:            testStore,
		githubAPIBaseURL: fakeAPI.server.URL,
		githubChatRepo:   defaultGitHubChatRepo,
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil)
	rr := httptest.NewRecorder()

	if err := server.listChats(rr, req); err != nil {
		t.Fatalf("listChats() error = %v", err)
	}

	var response responses.ChatsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.ChatInfos) != 1 {
		t.Fatalf("chat count = %d, want 1", len(response.ChatInfos))
	}
	if response.ChatInfos[0].ID != "remote-chat" {
		t.Fatalf("chat id = %q, want remote-chat", response.ChatInfos[0].ID)
	}
}

func TestGetChatLoadsMissingChatFromGitHub(t *testing.T) {
	fakeAPI := newFakeGitHubContentsAPI(t)
	testStore := &store.Store{DBPath: filepath.Join(t.TempDir(), "db.sqlite")}
	defer testStore.Close()

	if err := testStore.SetAuthSession(store.AuthSession{AccessToken: "gh-test-token"}); err != nil {
		t.Fatalf("SetAuthSession() error = %v", err)
	}

	chat := store.Chat{
		ID:        "remote-chat",
		Title:     "Remote chat",
		CreatedAt: time.Unix(1710000300, 0).UTC(),
		Messages: []store.Message{
			store.NewMessage("user", "loaded from github", nil),
		},
	}
	manifest := githubChatManifest{
		Chats:    []responses.ChatInfo{chatInfoFromChat(chat)},
		SyncedAt: time.Now().UTC(),
	}
	fakeAPI.putJSON(t, "chats/index.json", manifest)
	fakeAPI.putJSON(t, "chats/remote-chat.json", chat)

	server := &Server{
		Store:            testStore,
		githubAPIBaseURL: fakeAPI.server.URL,
		githubChatRepo:   defaultGitHubChatRepo,
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/remote-chat", nil)
	req.SetPathValue("id", "remote-chat")
	rr := httptest.NewRecorder()

	if err := server.getChat(rr, req); err != nil {
		t.Fatalf("getChat() error = %v", err)
	}

	var response responses.ChatResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Chat.ID != "remote-chat" {
		t.Fatalf("chat id = %q, want remote-chat", response.Chat.ID)
	}

	cached, err := testStore.Chat("remote-chat")
	if err != nil {
		t.Fatalf("cached chat lookup failed: %v", err)
	}
	if cached == nil || cached.ID != "remote-chat" {
		t.Fatalf("cached chat = %+v, want remote-chat", cached)
	}
}

type fakeGitHubContentsAPI struct {
	server            *httptest.Server
	mu                sync.Mutex
	files             map[string]fakeGitHubFile
	lastAuthorization string
}

type fakeGitHubFile struct {
	sha     string
	content []byte
}

func newFakeGitHubContentsAPI(t *testing.T) *fakeGitHubContentsAPI {
	t.Helper()

	api := &fakeGitHubContentsAPI{files: make(map[string]fakeGitHubFile)}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.handle(t, w, r)
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (f *fakeGitHubContentsAPI) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()

	prefix := "/repos/Oblivionevil/Chatrepo/contents/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, prefix)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAuthorization = r.Header.Get("Authorization")

	switch r.Method {
	case http.MethodGet:
		file, ok := f.files[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(githubContentsFile{
			Name:     filepath.Base(path),
			Path:     path,
			SHA:      file.sha,
			Type:     "file",
			Encoding: "base64",
			Content:  base64.StdEncoding.EncodeToString(file.content),
		})
	case http.MethodPut:
		var req githubContentsPutRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode PUT request: %v", err)
		}
		data, err := base64.StdEncoding.DecodeString(req.Content)
		if err != nil {
			t.Fatalf("decode PUT content: %v", err)
		}
		existing, ok := f.files[path]
		if ok && req.SHA != existing.sha {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"message": "sha mismatch"})
			return
		}
		sha := fakeGitHubSHA(path, data)
		f.files[path] = fakeGitHubFile{sha: sha, content: data}
		status := http.StatusCreated
		if ok {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"content": githubContentsFile{Path: path, SHA: sha}})
	case http.MethodDelete:
		var req githubContentsDeleteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode DELETE request: %v", err)
		}
		existing, ok := f.files[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if req.SHA != existing.sha {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"message": "sha mismatch"})
			return
		}
		delete(f.files, path)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeGitHubContentsAPI) putJSON(t *testing.T, path string, payload any) {
	t.Helper()

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = fakeGitHubFile{sha: fakeGitHubSHA(path, data), content: data}
}

func (f *fakeGitHubContentsAPI) fileContents(t *testing.T, path string) []byte {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()
	file, ok := f.files[path]
	if !ok {
		t.Fatalf("file %q was not written", path)
	}
	return file.content
}

func fakeGitHubSHA(path string, data []byte) string {
	hash := sha1.Sum([]byte(fmt.Sprintf("%s:%s", path, data)))
	return hex.EncodeToString(hash[:])
}
