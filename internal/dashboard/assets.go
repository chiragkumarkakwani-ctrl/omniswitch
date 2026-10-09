package dashboard

import (
	"bytes"
	"compress/gzip"
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
	gzipBody    []byte
	contentType string
	etag        string
	gzipETag    string
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
		// Keep both immutable representations. Compression is startup work, never
		// request work, and each representation has its own cache validator.
		compressed, compressErr := gzipAsset(body)
		if compressErr != nil {
			return compressErr
		}
		assets[strings.TrimPrefix(name, "static")] = dashboardAsset{
			body:        body,
			gzipBody:    compressed,
			contentType: contentType,
			etag:        fmt.Sprintf("\"%x\"", sha256.Sum256(body)),
			gzipETag:    fmt.Sprintf("\"%x-gzip\"", sha256.Sum256(compressed)),
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
				body, etag := asset.body, asset.etag
				w.Header().Add("Vary", "Accept-Encoding")
				if acceptsGzip(strings.Join(r.Header.Values("Accept-Encoding"), ",")) {
					body, etag = asset.gzipBody, asset.gzipETag
					w.Header().Set("Content-Encoding", "gzip")
				}
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("ETag", etag)
				w.Header().Set("Content-Type", asset.contentType)
				if matchesETag(r.Header.Get("If-None-Match"), etag) {
					w.WriteHeader(http.StatusNotModified)
					return
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				w.WriteHeader(http.StatusOK)
				if r.Method != http.MethodHead {
					_, _ = w.Write(body)
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

// gzipAsset uses the same standard-library compression for serving and budgets.
func gzipAsset(body []byte) ([]byte, error) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(body); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return compressed.Bytes(), nil
}

// An explicit gzip weight wins over a wildcard. Never send gzip when q=0,
// malformed, or absent; clients without compression keep the readable source.
func acceptsGzip(header string) bool {
	wildcard := false
	for _, entry := range strings.Split(header, ",") {
		parts := strings.Split(entry, ";")
		coding := strings.TrimSpace(parts[0])
		if !strings.EqualFold(coding, "gzip") && coding != "*" {
			continue
		}
		quality := 1.0
		for _, parameter := range parts[1:] {
			name, value, ok := strings.Cut(parameter, "=")
			if !strings.EqualFold(strings.TrimSpace(name), "q") {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if !ok || err != nil || !(parsed >= 0 && parsed <= 1) {
				quality = 0
				break
			}
			quality = parsed
		}
		if strings.EqualFold(coding, "gzip") {
			return quality > 0
		}
		wildcard = quality > 0
	}
	return wildcard
}
