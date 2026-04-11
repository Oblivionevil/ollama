//go:build windows || darwin || android

package ui

import (
	"bytes"
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

//go:embed app/dist
var appFS embed.FS

func (s *Server) authorizeDesktopAppRequest(w http.ResponseWriter, r *http.Request) bool {
	if s.Dev {
		return true
	}

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
		return false
	}

	cookie, err := r.Cookie("token")
	if err == nil && cookie.Value == s.Token {
		return true
	}

	http.Error(w, "Desktop UI is only available inside the app", http.StatusForbidden)
	return false
}

// appHandler returns an HTTP handler that serves the React SPA.
// It tries to serve real files first, then falls back to index.html for React Router.
func (s *Server) appHandler() http.Handler {
	// Strip the dist prefix so URLs look clean
	fsys, _ := fs.Sub(appFS, "app/dist")
	fileServer := http.FileServer(http.FS(fsys))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorizeDesktopAppRequest(w, r) {
			return
		}

		p := strings.TrimPrefix(r.URL.Path, "/")
		if _, err := fsys.Open(p); err == nil {
			// Serve the file directly
			fileServer.ServeHTTP(w, r)
			return
		}
		// Fallback – serve index.html for unknown paths so React Router works
		data, err := fs.ReadFile(fsys, "index.html")
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				http.NotFound(w, r)
			} else {
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
			return
		}
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(data))
	})
}
