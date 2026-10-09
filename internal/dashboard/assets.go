package dashboard

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

//go:embed static
var staticFiles embed.FS

const dashboardCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'; require-trusted-types-for 'script'"

type dashboardAsset struct {
	body        []byte
	contentType string
	etag        string
}

// PublicShell serves the data-free operator console ahead of the authenticated
// gateway chain. Only GET/HEAD shell and asset routes and GET /api/health are
// public. All API, protocol, metrics, and unknown routes remain owned by next.
// Mount it as topMux.Handle("/", dashboard.PublicShell(authenticatedChain)).
func PublicShell(next http.Handler) http.Handler {
	shell, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		panic(fmt.Sprintf("dashboard shell: %v", err))
	}
	assets := make(map[string]dashboardAsset)
	err = fs.WalkDir(staticFiles, "static/assets", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		body, readErr := staticFiles.ReadFile(name)
		if readErr != nil {
			return readErr
		}
		contentType := "application/octet-stream"
		switch path.Ext(name) {
		case ".js":
			contentType = "text/javascript; charset=utf-8"
		case ".css":
			contentType = "text/css; charset=utf-8"
		case ".svg":
			contentType = "image/svg+xml"
		}
		assets[strings.TrimPrefix(name, "static")] = dashboardAsset{
			body: body, contentType: contentType, etag: fmt.Sprintf("\"%x\"", sha256.Sum256(body)),
		}
		return nil
	})
	if err != nil {
		panic(fmt.Sprintf("dashboard assets: %v", err))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/health" {
			dashboardHeaders(w)
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusOK, map[string]any{"status": "healthy", "time": time.Now().UTC()})
			return
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			if destination, ok := legacyDashboardRoute(r.URL.Path); ok {
				query := r.URL.Query()
				if tab := strings.IndexByte(destination, '?'); tab >= 0 {
					query.Set("tab", strings.TrimPrefix(destination[tab+1:], "tab="))
					destination = destination[:tab]
				}
				if encoded := query.Encode(); encoded != "" {
					destination += "?" + encoded
				}
				dashboardHeaders(w)
				w.Header().Set("Cache-Control", "no-store")
				http.Redirect(w, r, destination, http.StatusPermanentRedirect)
				return
			}
			if isDashboardRoute(r.URL.Path) {
				dashboardHeaders(w)
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("Content-Length", strconv.Itoa(len(shell)))
				w.WriteHeader(http.StatusOK)
				if r.Method != http.MethodHead {
					_, _ = w.Write(shell)
				}
				return
			}
			if asset, ok := assets[r.URL.Path]; ok {
				dashboardHeaders(w)
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("ETag", asset.etag)
				w.Header().Set("Content-Type", asset.contentType)
				if matchesETag(r.Header.Get("If-None-Match"), asset.etag) {
					w.WriteHeader(http.StatusNotModified)
					return
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(asset.body)))
				w.WriteHeader(http.StatusOK)
				if r.Method != http.MethodHead {
					_, _ = w.Write(asset.body)
				}
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func dashboardHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", dashboardCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
}

func isDashboardRoute(route string) bool {
	switch route {
	case "/", "/signin", "/requests", "/guardrails", "/mcp", "/access", "/providers", "/playground", "/settings":
		return true
	default:
		return singleRouteID(route, "/requests/")
	}
}

func singleRouteID(route, prefix string) bool {
	id := strings.TrimPrefix(route, prefix)
	return strings.HasPrefix(route, prefix) && id != "" && id != "." && id != ".." && !strings.Contains(id, "/")
}

func legacyDashboardRoute(route string) (string, bool) {
	switch route {
	case "/overview":
		return "/", true
	case "/logs":
		return "/requests", true
	case "/keys":
		return "/access", true
	case "/configs":
		return "/settings?tab=configs", true
	case "/prompts":
		return "/playground?tab=prompts", true
	default:
		if singleRouteID(route, "/traces/") {
			return "/requests/" + url.PathEscape(strings.TrimPrefix(route, "/traces/")), true
		}
		return "", false
	}
}

func matchesETag(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}
