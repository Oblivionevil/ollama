//go:build windows || darwin || android

package ui

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/app/store"
	"github.com/ollama/ollama/app/types/not"
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
	if fakeAPI.lastAuthorization != "token gh-test-token" {
		t.Fatalf("authorization = %q, want token gh-test-token", fakeAPI.lastAuthorization)
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
	server := &Server{
		Store:            testStore,
		githubAPIBaseURL: fakeAPI.server.URL,
		githubChatRepo:   defaultGitHubChatRepo,
	}
	fakeAPI.putJSON(t, server.githubChatManifestPath(), manifest)

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
	server := &Server{
		Store:            testStore,
		githubAPIBaseURL: fakeAPI.server.URL,
		githubChatRepo:   defaultGitHubChatRepo,
	}
	fakeAPI.putJSON(t, server.githubChatManifestPath(), manifest)
	fakeAPI.putJSON(t, server.githubChatFilePath(chat.ID), chat)

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

	_, err := testStore.Chat("remote-chat")
	if !errors.Is(err, not.Found) {
		t.Fatalf("local cache error = %v, want not found", err)
	}
}

func TestListChatsMigratesLocalChatsToGitHubAndRemovesLocalCopy(t *testing.T) {
	fakeAPI := newFakeGitHubContentsAPI(t)
	testStore := &store.Store{DBPath: filepath.Join(t.TempDir(), "db.sqlite")}
	defer testStore.Close()

	if err := testStore.SetAuthSession(store.AuthSession{AccessToken: "gh-test-token"}); err != nil {
		t.Fatalf("SetAuthSession() error = %v", err)
	}

	chat := store.Chat{
		ID:        "local-chat",
		Title:     "Local chat",
		CreatedAt: time.Unix(1710000400, 0).UTC(),
		Messages: []store.Message{
			store.NewMessage("user", "migrate me", nil),
		},
	}
	if err := testStore.SetChat(chat); err != nil {
		t.Fatalf("SetChat() error = %v", err)
	}

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
	if response.ChatInfos[0].ID != chat.ID {
		t.Fatalf("chat id = %q, want %q", response.ChatInfos[0].ID, chat.ID)
	}

	rawChat := fakeAPI.fileContents(t, server.githubChatFilePath(chat.ID))
	var storedChat store.Chat
	if err := json.Unmarshal(rawChat, &storedChat); err != nil {
		t.Fatalf("unmarshal stored chat: %v", err)
	}
	if storedChat.ID != chat.ID {
		t.Fatalf("stored chat id = %q, want %q", storedChat.ID, chat.ID)
	}

	_, err := testStore.Chat(chat.ID)
	if !errors.Is(err, not.Found) {
		t.Fatalf("local cache error = %v, want not found", err)
	}
}

func TestGitHubChatAuthorizedSessionClearsSessionWithoutRepoAccess(t *testing.T) {
	fakeAPI := newFakeGitHubContentsAPI(t)
	fakeAPI.repoStatus = http.StatusNotFound

	testStore := &store.Store{DBPath: filepath.Join(t.TempDir(), "db.sqlite")}
	defer testStore.Close()

	if err := testStore.SetAuthSession(store.AuthSession{AccessToken: "gh-test-token"}); err != nil {
		t.Fatalf("SetAuthSession() error = %v", err)
	}

	server := &Server{
		Store:            testStore,
		githubAPIBaseURL: fakeAPI.server.URL,
		githubChatRepo:   defaultGitHubChatRepo,
	}

	_, err := server.githubChatAuthorizedSession(context.Background())
	if err == nil {
		t.Fatal("githubChatAuthorizedSession() error = nil, want authorization error")
	}

	var authErr api.AuthorizationError
	if !errors.As(err, &authErr) {
		t.Fatalf("githubChatAuthorizedSession() error = %T, want authorization error", err)
	}

	session, err := testStore.AuthSession()
	if err != nil {
		t.Fatalf("AuthSession() error = %v", err)
	}
	if session != nil {
		t.Fatal("auth session was not cleared after repo access failure")
	}
}

func TestDeleteAllChatsRemovesRemoteAndLocalChats(t *testing.T) {
	fakeAPI := newFakeGitHubContentsAPI(t)
	testStore := &store.Store{DBPath: filepath.Join(t.TempDir(), "db.sqlite")}
	defer testStore.Close()

	if err := testStore.SetAuthSession(store.AuthSession{AccessToken: "gh-test-token"}); err != nil {
		t.Fatalf("SetAuthSession() error = %v", err)
	}

	localChat := store.Chat{
		ID:        "local-chat",
		Title:     "Local chat",
		CreatedAt: time.Unix(1710000500, 0).UTC(),
		Messages: []store.Message{
			store.NewMessage("user", "delete me too", nil),
		},
	}
	if err := testStore.SetChat(localChat); err != nil {
		t.Fatalf("SetChat() error = %v", err)
	}

	remoteChat := store.Chat{
		ID:        "remote-chat",
		Title:     "Remote chat",
		CreatedAt: time.Unix(1710000600, 0).UTC(),
		Messages: []store.Message{
			store.NewMessage("user", "remove me", nil),
		},
	}
	manifest := githubChatManifest{
		Chats: []responses.ChatInfo{chatInfoFromChat(remoteChat)},
		SyncedAt: time.Now().UTC(),
	}

	server := &Server{
		Store:            testStore,
		githubAPIBaseURL: fakeAPI.server.URL,
		githubChatRepo:   defaultGitHubChatRepo,
	}
	fakeAPI.putJSON(t, server.githubChatManifestPath(), manifest)
	fakeAPI.putJSON(t, server.githubChatFilePath(remoteChat.ID), remoteChat)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/chats", nil)
	rr := httptest.NewRecorder()

	if err := server.deleteAllChats(rr, req); err != nil {
		t.Fatalf("deleteAllChats() error = %v", err)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	manifestData := fakeAPI.fileContents(t, server.githubChatManifestPath())
	var updatedManifest githubChatManifest
	if err := json.Unmarshal(manifestData, &updatedManifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if len(updatedManifest.Chats) != 0 {
		t.Fatalf("manifest chat count = %d, want 0", len(updatedManifest.Chats))
	}

	if fakeAPI.hasFile(server.githubChatFilePath(remoteChat.ID)) {
		t.Fatalf("remote chat file %q still exists", remoteChat.ID)
	}
	if fakeAPI.hasFile(server.githubChatFilePath(localChat.ID)) {
		t.Fatalf("migrated local chat file %q still exists", localChat.ID)
	}

	chatInfos, err := testStore.Chats()
	if err != nil {
		t.Fatalf("Chats() error = %v", err)
	}
	if len(chatInfos) != 0 {
		t.Fatalf("local chat count = %d, want 0", len(chatInfos))
	}
}

func TestDeleteAllChatsHonorsRequestContext(t *testing.T) {
	fakeAPI := newFakeGitHubContentsAPI(t)
	fakeAPI.requestDelay = 100 * time.Millisecond

	testStore := &store.Store{DBPath: filepath.Join(t.TempDir(), "db.sqlite")}
	defer testStore.Close()

	if err := testStore.SetAuthSession(store.AuthSession{AccessToken: "gh-test-token"}); err != nil {
		t.Fatalf("SetAuthSession() error = %v", err)
	}

	chatA := store.Chat{
		ID:        "chat-a",
		Title:     "Chat A",
		CreatedAt: time.Unix(1710000700, 0).UTC(),
		Messages: []store.Message{
			store.NewMessage("user", "a", nil),
		},
	}
	chatB := store.Chat{
		ID:        "chat-b",
		Title:     "Chat B",
		CreatedAt: time.Unix(1710000800, 0).UTC(),
		Messages: []store.Message{
			store.NewMessage("user", "b", nil),
		},
	}

	server := &Server{
		Store:            testStore,
		githubAPIBaseURL: fakeAPI.server.URL,
		githubChatRepo:   defaultGitHubChatRepo,
	}
	fakeAPI.putJSON(t, server.githubChatManifestPath(), githubChatManifest{
		Chats: []responses.ChatInfo{chatInfoFromChat(chatA), chatInfoFromChat(chatB)},
		SyncedAt: time.Now().UTC(),
	})
	fakeAPI.putJSON(t, server.githubChatFilePath(chatA.ID), chatA)
	fakeAPI.putJSON(t, server.githubChatFilePath(chatB.ID), chatB)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/chats", nil).WithContext(ctx)
	rr := httptest.NewRecorder()

	err := server.deleteAllChats(rr, req)
	if err == nil {
		t.Fatal("deleteAllChats() error = nil, want context deadline exceeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("deleteAllChats() error = %v, want context deadline exceeded", err)
	}
}

func TestListChatsReturnsEmptyWhenGitHubValidationFails(t *testing.T) {
	fakeAPI := newFakeGitHubContentsAPI(t)
	fakeAPI.repoStatus = http.StatusInternalServerError

	testStore := &store.Store{DBPath: filepath.Join(t.TempDir(), "db.sqlite")}
	defer testStore.Close()

	if err := testStore.SetAuthSession(store.AuthSession{AccessToken: "gh-test-token"}); err != nil {
		t.Fatalf("SetAuthSession() error = %v", err)
	}

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
	if len(response.ChatInfos) != 0 {
		t.Fatalf("chat count = %d, want 0", len(response.ChatInfos))
	}

	session, err := testStore.AuthSession()
	if err != nil {
		t.Fatalf("AuthSession() error = %v", err)
	}
	if session == nil {
		t.Fatal("auth session should be preserved for transient GitHub failures")
	}
}

type fakeGitHubContentsAPI struct {
	server            *httptest.Server
	mu                sync.Mutex
	files             map[string]fakeGitHubFile
	lastAuthorization string
	repoStatus        int
	requestDelay      time.Duration
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

	if f.requestDelay > 0 {
		select {
		case <-time.After(f.requestDelay):
		case <-r.Context().Done():
			return
		}
	}

	if r.URL.Path == "/repos/Oblivionevil/Chatrepo" {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastAuthorization = r.Header.Get("Authorization")
		if f.repoStatus != 0 && f.repoStatus != http.StatusOK {
			w.WriteHeader(f.repoStatus)
			json.NewEncoder(w).Encode(map[string]string{"message": http.StatusText(f.repoStatus)})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"full_name": defaultGitHubChatRepo})
		return
	}

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

func (f *fakeGitHubContentsAPI) hasFile(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[path]
	return ok
}

func fakeGitHubSHA(path string, data []byte) string {
	hash := sha1.Sum([]byte(fmt.Sprintf("%s:%s", path, data)))
	return hex.EncodeToString(hash[:])
}
