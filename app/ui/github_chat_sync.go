//go:build windows || darwin || android

package ui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/app/store"
	"github.com/ollama/ollama/app/types/not"
	"github.com/ollama/ollama/app/ui/responses"
)

const (
	defaultGitHubChatRepo = "Oblivionevil/Chatrepo"
	githubChatFolder      = "Chat"
)

type githubContentsFile struct {
	Name     string `json:"name,omitempty"`
	Path     string `json:"path,omitempty"`
	SHA      string `json:"sha,omitempty"`
	Type     string `json:"type,omitempty"`
	Encoding string `json:"encoding,omitempty"`
	Content  string `json:"content,omitempty"`
}

type githubContentsPutRequest struct {
	Message string `json:"message"`
	Content string `json:"content"`
	SHA     string `json:"sha,omitempty"`
	Branch  string `json:"branch,omitempty"`
}

type githubContentsDeleteRequest struct {
	Message string `json:"message"`
	SHA     string `json:"sha"`
	Branch  string `json:"branch,omitempty"`
}

type githubChatManifest struct {
	Chats    []responses.ChatInfo `json:"chats"`
	SyncedAt time.Time            `json:"synced_at,omitempty"`
}

func (s *Server) listChatInfos(ctx context.Context) ([]responses.ChatInfo, error) {
	session, err := s.githubChatAuthorizedSession(ctx)
	if err != nil {
		if isAuthorizationError(err) {
			return []responses.ChatInfo{}, nil
		}
		s.log().Warn("failed to validate GitHub chat session", "error", err)
		return []responses.ChatInfo{}, nil
	}

	if err := s.migrateLocalChatsToGitHub(ctx, session); err != nil {
		s.log().Warn("failed to migrate local chats to GitHub", "error", err)
	}

	manifest, err := s.loadGitHubChatManifest(ctx, session)
	if err != nil {
		s.log().Warn("failed to load GitHub chat manifest", "error", err)
		return []responses.ChatInfo{}, nil
	}
	sortManifest(manifest)

	return append([]responses.ChatInfo(nil), manifest.Chats...), nil
}

func (s *Server) loadChatFromGitHub(ctx context.Context, session *store.AuthSession, chatID string) (*store.Chat, error) {
	if err := s.migrateLocalChatToGitHub(ctx, session, chatID); err != nil {
		return nil, err
	}

	remote, found, err := s.loadGitHubChat(ctx, session, chatID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: chat %s", not.Found, chatID)
	}

	return remote, nil
}

func (s *Server) storeChatRemotely(session *store.AuthSession, chat store.Chat) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := s.syncChatToGitHub(ctx, session, chat); err != nil {
		return err
	}

	return s.deleteLocalChatCache(chat.ID)
}

func (s *Server) deleteChatRemotely(session *store.AuthSession, chatID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := s.deleteChatFromGitHub(ctx, session, chatID); err != nil {
		return err
	}

	return s.deleteLocalChatCache(chatID)
}

func (s *Server) deleteLocalChatCache(chatID string) error {
	if s.Store == nil {
		return nil
	}

	err := s.Store.DeleteChat(chatID)
	if err != nil && !errors.Is(err, not.Found) {
		return err
	}

	return nil
}

func (s *Server) migrateLocalChatsToGitHub(ctx context.Context, session *store.AuthSession) error {
	if s.Store == nil {
		return nil
	}

	localChats, err := s.Store.Chats()
	if err != nil {
		return err
	}

	for _, localChat := range localChats {
		if err := s.migrateLocalChatToGitHub(ctx, session, localChat.ID); err != nil {
			return err
		}
	}

	return nil
}

func (s *Server) migrateLocalChatToGitHub(ctx context.Context, session *store.AuthSession, chatID string) error {
	if s.Store == nil {
		return nil
	}

	localChat, err := s.Store.ChatWithOptions(chatID, true)
	if err != nil {
		if errors.Is(err, not.Found) {
			return nil
		}
		return err
	}

	manifest, err := s.loadGitHubChatManifest(ctx, session)
	if err != nil {
		return err
	}

	localInfo := chatInfoFromChat(*localChat)
	remoteInfo, found := manifest.chatInfo(chatID)
	if !found || localInfo.UpdatedAt.After(remoteInfo.UpdatedAt) {
		if err := s.syncChatToGitHub(ctx, session, *localChat); err != nil {
			return err
		}
	}

	return s.deleteLocalChatCache(chatID)
}

func (s *Server) syncChatToGitHubBestEffort(_ context.Context, chat store.Chat) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	session, err := s.githubChatAuthorizedSession(ctx)
	if err != nil {
		if !isAuthorizationError(err) {
			s.log().Warn("failed to load GitHub chat session", "chat_id", chat.ID, "error", err)
		}
		return
	}

	if err := s.syncChatToGitHub(ctx, session, chat); err != nil {
		s.log().Warn("failed to sync chat to GitHub", "chat_id", chat.ID, "error", err)
	}
}

func (s *Server) deleteChatFromGitHubBestEffort(_ context.Context, chatID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	session, err := s.githubChatAuthorizedSession(ctx)
	if err != nil {
		if !isAuthorizationError(err) {
			s.log().Warn("failed to load GitHub chat session", "chat_id", chatID, "error", err)
		}
		return
	}

	if err := s.deleteChatFromGitHub(ctx, session, chatID); err != nil {
		s.log().Warn("failed to delete chat from GitHub", "chat_id", chatID, "error", err)
	}
}

func (s *Server) syncChatToGitHub(ctx context.Context, session *store.AuthSession, chat store.Chat) error {
	if err := s.upsertGitHubJSONFile(ctx, session, s.githubChatFilePath(chat.ID), chat, fmt.Sprintf("Sync chat %s", chat.ID)); err != nil {
		return err
	}

	manifest, err := s.loadGitHubChatManifest(ctx, session)
	if err != nil {
		return err
	}

	info := chatInfoFromChat(chat)
	updated := false
	for i := range manifest.Chats {
		if manifest.Chats[i].ID == info.ID {
			manifest.Chats[i] = info
			updated = true
			break
		}
	}
	if !updated {
		manifest.Chats = append(manifest.Chats, info)
	}
	manifest.SyncedAt = time.Now().UTC()
	sortManifest(manifest)

	return s.upsertGitHubJSONFile(ctx, session, s.githubChatManifestPath(), manifest, fmt.Sprintf("Update chat index for %s", chat.ID))
}

func (s *Server) deleteChatFromGitHub(ctx context.Context, session *store.AuthSession, chatID string) error {
	manifest, err := s.loadGitHubChatManifest(ctx, session)
	if err != nil {
		return err
	}

	filtered := manifest.Chats[:0]
	removed := false
	for _, info := range manifest.Chats {
		if info.ID == chatID {
			removed = true
			continue
		}
		filtered = append(filtered, info)
	}
	manifest.Chats = filtered
	manifest.SyncedAt = time.Now().UTC()

	if err := s.deleteGitHubFile(ctx, session, s.githubChatFilePath(chatID), fmt.Sprintf("Delete chat %s", chatID)); err != nil {
		return err
	}

	if removed || len(manifest.Chats) > 0 {
		sortManifest(manifest)
		if err := s.upsertGitHubJSONFile(ctx, session, s.githubChatManifestPath(), manifest, fmt.Sprintf("Update chat index for %s", chatID)); err != nil {
			return err
		}
	}

	return nil
}

func (s *Server) loadGitHubChatManifest(ctx context.Context, session *store.AuthSession) (*githubChatManifest, error) {
	file, found, err := s.githubGetContentsFile(ctx, session, s.githubChatManifestPath())
	if err != nil {
		return nil, err
	}
	if !found {
		return &githubChatManifest{Chats: []responses.ChatInfo{}}, nil
	}

	data, err := decodeGitHubFileContent(file.Content)
	if err != nil {
		return nil, fmt.Errorf("decode GitHub chat manifest: %w", err)
	}

	var manifest githubChatManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse GitHub chat manifest: %w", err)
	}
	if manifest.Chats == nil {
		manifest.Chats = []responses.ChatInfo{}
	}

	return &manifest, nil
}

func (s *Server) loadGitHubChat(ctx context.Context, session *store.AuthSession, chatID string) (*store.Chat, bool, error) {
	file, found, err := s.githubGetContentsFile(ctx, session, s.githubChatFilePath(chatID))
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}

	data, err := decodeGitHubFileContent(file.Content)
	if err != nil {
		return nil, false, fmt.Errorf("decode GitHub chat %s: %w", chatID, err)
	}

	var chat store.Chat
	if err := json.Unmarshal(data, &chat); err != nil {
		return nil, false, fmt.Errorf("parse GitHub chat %s: %w", chatID, err)
	}

	return &chat, true, nil
}

func (s *Server) githubChatAuthorizedSession(ctx context.Context) (*store.AuthSession, error) {
	if s.Store == nil {
		return nil, api.AuthorizationError{StatusCode: http.StatusUnauthorized, Status: http.StatusText(http.StatusUnauthorized)}
	}

	session, err := s.Store.AuthSession()
	if err != nil {
		return nil, err
	}
	if session == nil || session.AccessToken == "" {
		return nil, api.AuthorizationError{StatusCode: http.StatusUnauthorized, Status: http.StatusText(http.StatusUnauthorized)}
	}
	if err := s.ensureGitHubChatRepoAccess(ctx, session); err != nil {
		return nil, err
	}

	return session, nil
}

func (s *Server) githubChatRepoSpec() string {
	if s.githubChatRepo != "" {
		return s.githubChatRepo
	}
	if repo := strings.TrimSpace(os.Getenv("OLLAMA_CHAT_SYNC_REPO")); repo != "" {
		return repo
	}
	return defaultGitHubChatRepo
}

func (s *Server) githubChatRepoParts() (string, string, error) {
	repo := strings.TrimSpace(s.githubChatRepoSpec())
	repo = strings.TrimSuffix(repo, ".git")
	repo = strings.TrimPrefix(repo, "https://github.com/")
	repo = strings.Trim(repo, "/")

	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid GitHub chat repository %q", s.githubChatRepoSpec())
	}

	return parts[0], parts[1], nil
}

func (s *Server) githubAPIBase() string {
	if s.githubAPIBaseURL != "" {
		return strings.TrimRight(s.githubAPIBaseURL, "/")
	}
	return "https://api.github.com"
}

func (s *Server) ensureGitHubChatRepoAccess(ctx context.Context, session *store.AuthSession) error {
	endpoint, err := s.githubRepoEndpoint()
	if err != nil {
		return err
	}

	resp, data, err := s.doJSONRequest(ctx, http.MethodGet, endpoint, s.githubChatHeaders(session), nil)
	if err != nil {
		return fmt.Errorf("validate GitHub chat repository access: %w", err)
	}
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		s.copilotClearAuthSession()
		return api.AuthorizationError{
			StatusCode: http.StatusUnauthorized,
			Status:     "GitHub chat sync requires signing in again to grant repository access",
		}
	}

	return fmt.Errorf("validate GitHub chat repository access failed: %s", strings.TrimSpace(string(data)))
}

func (s *Server) githubChatHeaders(session *store.AuthSession) map[string]string {
	return map[string]string{
		"Authorization":        "token " + session.AccessToken,
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": githubAPIVersion,
		"User-Agent":           userAgent(),
	}
}

func (s *Server) githubGetContentsFile(ctx context.Context, session *store.AuthSession, filePath string) (*githubContentsFile, bool, error) {
	endpoint, err := s.githubContentsEndpoint(filePath)
	if err != nil {
		return nil, false, err
	}

	resp, data, err := s.doJSONRequest(ctx, http.MethodGet, endpoint, s.githubChatHeaders(session), nil)
	if err != nil {
		return nil, false, fmt.Errorf("fetch %s: %w", filePath, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, false, fmt.Errorf("fetch %s from GitHub failed: %s", filePath, strings.TrimSpace(string(data)))
	}

	var file githubContentsFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, false, fmt.Errorf("parse %s from GitHub: %w", filePath, err)
	}

	return &file, true, nil
}

func (s *Server) upsertGitHubJSONFile(ctx context.Context, session *store.AuthSession, filePath string, payload any, message string) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s for GitHub: %w", filePath, err)
	}

	existing, found, err := s.githubGetContentsFile(ctx, session, filePath)
	if err != nil {
		return err
	}

	body := githubContentsPutRequest{
		Message: message,
		Content: base64.StdEncoding.EncodeToString(data),
	}
	if found {
		body.SHA = existing.SHA
	}

	endpoint, err := s.githubContentsEndpoint(filePath)
	if err != nil {
		return err
	}

	resp, responseData, err := s.doJSONRequest(ctx, http.MethodPut, endpoint, s.githubChatHeaders(session), body)
	if err != nil {
		return fmt.Errorf("sync %s to GitHub: %w", filePath, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("sync %s to GitHub failed: %s", filePath, strings.TrimSpace(string(responseData)))
	}

	return nil
}

func (s *Server) deleteGitHubFile(ctx context.Context, session *store.AuthSession, filePath string, message string) error {
	existing, found, err := s.githubGetContentsFile(ctx, session, filePath)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}

	body := githubContentsDeleteRequest{
		Message: message,
		SHA:     existing.SHA,
	}

	endpoint, err := s.githubContentsEndpoint(filePath)
	if err != nil {
		return err
	}

	resp, responseData, err := s.doJSONRequest(ctx, http.MethodDelete, endpoint, s.githubChatHeaders(session), body)
	if err != nil {
		return fmt.Errorf("delete %s from GitHub: %w", filePath, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("delete %s from GitHub failed: %s", filePath, strings.TrimSpace(string(responseData)))
	}

	return nil
}

func (s *Server) githubContentsEndpoint(filePath string) (string, error) {
	owner, repo, err := s.githubChatRepoParts()
	if err != nil {
		return "", err
	}

	filePath = strings.TrimPrefix(filePath, "/")
	return fmt.Sprintf("%s/repos/%s/%s/contents/%s", s.githubAPIBase(), owner, repo, filePath), nil
}

func (s *Server) githubRepoEndpoint() (string, error) {
	owner, repo, err := s.githubChatRepoParts()
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s/repos/%s/%s", s.githubAPIBase(), owner, repo), nil
}

func (s *Server) githubChatFilePath(chatID string) string {
	return fmt.Sprintf("%s/%s.json", githubChatFolder, chatID)
}

func (s *Server) githubChatManifestPath() string {
	return fmt.Sprintf("%s/index.json", githubChatFolder)
}

func decodeGitHubFileContent(content string) ([]byte, error) {
	clean := strings.ReplaceAll(content, "\n", "")
	clean = strings.ReplaceAll(clean, "\r", "")
	if clean == "" {
		return nil, nil
	}
	return base64.StdEncoding.DecodeString(clean)
}

func sortManifest(manifest *githubChatManifest) {
	sort.SliceStable(manifest.Chats, func(i, j int) bool {
		if manifest.Chats[i].UpdatedAt.Equal(manifest.Chats[j].UpdatedAt) {
			return manifest.Chats[i].CreatedAt.After(manifest.Chats[j].CreatedAt)
		}
		return manifest.Chats[i].UpdatedAt.After(manifest.Chats[j].UpdatedAt)
	})
}

func (m *githubChatManifest) chatInfo(chatID string) (responses.ChatInfo, bool) {
	for _, info := range m.Chats {
		if info.ID == chatID {
			return info, true
		}
	}
	return responses.ChatInfo{}, false
}

func isAuthorizationError(err error) bool {
	var authErr api.AuthorizationError
	return errors.As(err, &authErr)
}
