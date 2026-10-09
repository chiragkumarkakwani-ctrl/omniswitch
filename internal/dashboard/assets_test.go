package dashboard

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/gateway"
)

func TestPublicShellHeadersAndMethods(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Next", "true")
		w.WriteHeader(http.StatusUnauthorized)
	})
	handler := PublicShell(next)
	for _, route := range []string{"/", "/signin", "/requests", "/requests/trace_1", "/guardrails", "/mcp", "/access", "/providers", "/playground", "/settings"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method+" "+route, func(t *testing.T) {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(method, route, nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
				}
				assertDashboardHeaders(t, rec)
				if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Type") != "text/html; charset=utf-8" {
					t.Fatalf("shell cache/type = %v", rec.Header())
				}
				if rec.Header().Get("Content-Length") == "" || rec.Header().Get("ETag") != "" {
					t.Fatal("shell must have a length and no cache validator")
				}
				if method == http.MethodHead && rec.Body.Len() != 0 {
					t.Fatal("HEAD returned a body")
				}
				if method == http.MethodGet && !strings.Contains(rec.Body.String(), `data-console-shell="omniswitch"`) {
					t.Fatal("not the data-free console shell")
				}
			})
		}
	}
	for _, route := range []string{"/", "/signin", "/requests/trace_1", "/logs", "/assets/app.js", "/api/health"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, route, nil))
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("X-Next") != "true" {
			t.Errorf("POST %s bypassed next: %d", route, rec.Code)
		}
	}
}

func TestPublicShellAssetRevalidation(t *testing.T) {
	handler := PublicShell(http.NotFoundHandler())
	for _, asset := range []struct{ route, contentType string }{
		{"/assets/app.js", "text/javascript; charset=utf-8"},
		{"/assets/app.css", "text/css; charset=utf-8"},
		{"/assets/shield.svg", "image/svg+xml"},
	} {
		t.Run(asset.route, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, asset.route, nil))
			assertDashboardHeaders(t, rec)
			if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-cache" || rec.Header().Get("Content-Type") != asset.contentType {
				t.Fatalf("asset response: %d %v", rec.Code, rec.Header())
			}
			sum := sha256.Sum256(rec.Body.Bytes())
			etag := `"` + hex.EncodeToString(sum[:]) + `"`
			if rec.Header().Get("ETag") != etag {
				t.Fatalf("ETag = %q, want %q", rec.Header().Get("ETag"), etag)
			}
			for _, validator := range []string{etag, "W/" + etag, `"not-this-file", W/` + etag, "*"} {
				request := httptest.NewRequest(http.MethodGet, asset.route, nil)
				request.Header.Set("If-None-Match", validator)
				cached := httptest.NewRecorder()
				handler.ServeHTTP(cached, request)
				assertDashboardHeaders(t, cached)
				if cached.Code != http.StatusNotModified || cached.Body.Len() != 0 || cached.Header().Get("ETag") != etag {
					t.Errorf("validator %q: %d %s", validator, cached.Code, cached.Body.String())
				}
			}
			head := httptest.NewRecorder()
			handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, asset.route, nil))
			if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != rec.Header().Get("Content-Length") {
				t.Fatal("asset HEAD differs from GET")
			}
			request := httptest.NewRequest(http.MethodGet, asset.route, nil)
			request.Header.Set("If-None-Match", `"old-version"`)
			fresh := httptest.NewRecorder()
			handler.ServeHTTP(fresh, request)
			if fresh.Code != http.StatusOK || fresh.Body.Len() == 0 {
				t.Fatal("stale validator did not reload asset")
			}
		})
	}
}

func TestPublicShellAssetGzip(t *testing.T) {
	handler := PublicShell(http.NotFoundHandler())
	for _, route := range []string{"/assets/app.js", "/assets/app.css", "/assets/shield.svg"} {
		t.Run(route, func(t *testing.T) {
			identity := httptest.NewRecorder()
			handler.ServeHTTP(identity, httptest.NewRequest(http.MethodGet, route, nil))
			if identity.Header().Get("Vary") != "Accept-Encoding" || identity.Header().Get("Content-Encoding") != "" {
				t.Fatal("identity response must vary by encoding without an encoding header")
			}
			request := httptest.NewRequest(http.MethodGet, route, nil)
			request.Header.Add("Accept-Encoding", "br")
			request.Header.Add("Accept-Encoding", "gzip")
			compressed := httptest.NewRecorder()
			handler.ServeHTTP(compressed, request)
			assertDashboardHeaders(t, compressed)
			if compressed.Code != http.StatusOK || compressed.Header().Get("Content-Encoding") != "gzip" || compressed.Header().Get("Vary") != "Accept-Encoding" {
				t.Fatalf("gzip response: %d %v", compressed.Code, compressed.Header())
			}
			if compressed.Header().Get("Content-Type") != identity.Header().Get("Content-Type") || compressed.Header().Get("Content-Length") != strconv.Itoa(compressed.Body.Len()) {
				t.Fatal("gzip content type or encoded content length is incorrect")
			}
			reader, err := gzip.NewReader(bytes.NewReader(compressed.Body.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := io.ReadAll(reader)
			if closeErr := reader.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if err != nil || !bytes.Equal(decoded, identity.Body.Bytes()) {
				t.Fatalf("decoded gzip differs from readable source: %v", err)
			}
			gzipETag := compressed.Header().Get("ETag")
			identityETag := identity.Header().Get("ETag")
			sum := sha256.Sum256(compressed.Body.Bytes())
			if gzipETag != `"`+hex.EncodeToString(sum[:])+`-gzip"` || gzipETag == identityETag {
				t.Fatalf("gzip ETag = %q, identity ETag = %q", gzipETag, identityETag)
			}
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				for _, encoding := range []string{"", "gzip"} {
					etag := identityETag
					otherETag := gzipETag
					if encoding == "gzip" {
						etag, otherETag = otherETag, etag
					}
					request := httptest.NewRequest(method, route, nil)
					request.Header.Set("Accept-Encoding", encoding)
					request.Header.Set("If-None-Match", "W/"+etag)
					cached := httptest.NewRecorder()
					handler.ServeHTTP(cached, request)
					if cached.Code != http.StatusNotModified || cached.Body.Len() != 0 || cached.Header().Get("ETag") != etag || cached.Header().Get("Vary") != "Accept-Encoding" || cached.Header().Get("Content-Encoding") != encoding {
						t.Fatalf("%s %s revalidation: %d %v", method, encoding, cached.Code, cached.Header())
					}
					request.Header.Set("If-None-Match", otherETag)
					fresh := httptest.NewRecorder()
					handler.ServeHTTP(fresh, request)
					if fresh.Code != http.StatusOK || fresh.Header().Get("ETag") != etag {
						t.Fatal("validator from the other encoding reused a representation")
					}
					if method == http.MethodHead && fresh.Body.Len() != 0 {
						t.Fatal("gzip or identity HEAD returned a body")
					}
				}
			}
			request = httptest.NewRequest(http.MethodGet, route, nil)
			request.Header.Set("Accept-Encoding", "gzip;q=0, *;q=1")
			disabled := httptest.NewRecorder()
			handler.ServeHTTP(disabled, request)
			if disabled.Header().Get("Content-Encoding") != "" || !bytes.Equal(disabled.Body.Bytes(), identity.Body.Bytes()) {
				t.Fatal("explicit gzip;q=0 must select the identity representation")
			}
		})
	}
}

func TestAcceptsGzip(t *testing.T) {
	for header, want := range map[string]bool{
		"": false, "br, deflate": false, "gzip": true, "br, GZIP; q=0.5": true,
		"gzip;q=0": false, "gzip;q=0, *;q=1": false, "*;q=1, gzip;q=0": false,
		"*;q=0.5": true, "*;q=0": false, "gzip;q=1.1": false,
		"gzip;q=-1": false, "gzip;q=no": false, "gzip;q=NaN": false,
		"gzip;q": false, "gzip;q=0.001": true, "x-gzip": false,
	} {
		if got := acceptsGzip(header); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}

func TestPublicShellLegacyRedirects(t *testing.T) {
	handler := PublicShell(http.NotFoundHandler())
	for from, to := range map[string]string{
		"/overview?window=6h": "/?window=6h", "/logs?q=test": "/requests?q=test", "/keys": "/access",
		"/configs?window=7d": "/settings?tab=configs&window=7d", "/prompts": "/playground?tab=prompts",
		"/traces/trace_1": "/requests/trace_1", "/traces/a%3Fb": "/requests/a%3Fb",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(method, from, nil))
			if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != to {
				t.Errorf("%s %s: %d %q, want %q", method, from, rec.Code, rec.Header().Get("Location"), to)
			}
		}
	}
}

func TestPublicShellDoesNotBroadenAuthentication(t *testing.T) {
	st := newDashboardTestStore(t)
	mux := http.NewServeMux()
	New(st).RegisterRoutes(mux)
	auth := gateway.NewAuthMiddleware(st, true)
	handler := PublicShell(auth.Wrap(gateway.NewAuthorizationMiddleware(true).Wrap(mux)))
	for _, route := range []string{"/", "/assets/app.js", "/api/health"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, route, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("public %s = %d", route, rec.Code)
		}
	}
	for _, route := range []string{"/api/session", "/api/logs", "/api/keys", "/api/retention", "/api/config/raw", "/metrics", "/v1/models", "/mcp/", "/a2a", "/unknown", "/requests/a/b", "/requests/", "/assets/missing.js", "/index.html", "/assets/../index.html"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, route, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("private %s = %d, body = %s", route, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/api/health", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatal("only GET /api/health is exempt")
	}
	for _, route := range []string{"/", "/unknown", "/assets/app.js"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, route, nil))
		var response map[string]any
		if rec.Code != http.StatusNotFound || json.Unmarshal(rec.Body.Bytes(), &response) != nil || response["error"] == nil {
			t.Errorf("bare authenticated mux %s must return JSON 404: %d %s", route, rec.Code, rec.Body.String())
		}
	}
}

func TestEmbeddedConsoleConformance(t *testing.T) {
	inlineScript := regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script\s*>`)
	inlineStyle := regexp.MustCompile(`(?i)<style\b|\bstyle\s*=|\bon[a-z]+\s*=`)
	unsafeJS := regexp.MustCompile(`\b(?:innerHTML|outerHTML|insertAdjacentHTML)\b|\bdocument\s*\.\s*write\s*\(|\beval\s*\(|\bnew\s+Function\b|\blocalStorage\b`)
	externalURL := regexp.MustCompile(`(?i)https?://`)
	imports := regexp.MustCompile(`(?m)^\s*(?:import|export)\s+(?:[^;"']*?\bfrom\s*)?["']([^"']+)["']`)
	dynamicImports := regexp.MustCompile(`\bimport\s*\(\s*["']([^"']+)["']\s*\)`)
	htmlAssets := regexp.MustCompile(`\b(?:src|href)\s*=\s*["'](/assets/[^"']+)["']`)
	gzipByExtension := make(map[string]int)
	totalGzip, iconsGzip, totalRaw := 0, 0, 0
	err := fs.WalkDir(staticFiles, "static", func(name string, entry fs.DirEntry, walkErr error) error {
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
		text := string(body)
		extension := path.Ext(name)
		compressed, compressErr := gzipAsset(body)
		if compressErr != nil {
			return compressErr
		}
		gzipByExtension[extension] += len(compressed)
		totalGzip += len(compressed)
		totalRaw += len(body)
		if extension == ".svg" || path.Base(name) == "icons.js" {
			iconsGzip += len(compressed)
		}
		for lineNumber, line := range strings.Split(text, "\n") {
			if len(line) > 120 {
				t.Errorf("%s:%d has a long source line (%d bytes)", name, lineNumber+1, len(line))
			}
		}
		if (extension == ".html" || extension == ".svg") && inlineStyle.MatchString(text) {
			t.Errorf("%s has inline styles or event attributes", name)
		}
		if unsafeJS.MatchString(text) {
			t.Errorf("%s has an unsafe DOM/code sink or local storage", name)
		}
		// SVG's XML namespace is an identifier, not an external fetch URL.
		withoutNamespace := strings.ReplaceAll(text, "http://www.w3.org/2000/svg", "")
		if externalURL.MatchString(withoutNamespace) {
			t.Errorf("%s has an external HTTP(S) URL", name)
		}
		if strings.Contains(text, "sessionStorage") && name != "static/assets/lib/api.js" {
			t.Errorf("%s handles tab credentials outside the API closure", name)
		}
		for _, script := range inlineScript.FindAllStringSubmatch(text, -1) {
			if !strings.Contains(script[1], `type="module"`) || !strings.Contains(script[1], `src="/assets/`) || strings.TrimSpace(script[2]) != "" {
				t.Errorf("%s has a non-module or inline script", name)
			}
		}
		if extension == ".css" && (strings.Contains(text, "@import") || strings.Contains(text, "url(")) {
			t.Errorf("%s uses external CSS imports/resources", name)
		}
		if extension == ".js" {
			matches := imports.FindAllStringSubmatch(text, -1)
			matches = append(matches, dynamicImports.FindAllStringSubmatch(text, -1)...)
			for _, match := range matches {
				importPath := match[1]
				if !strings.HasPrefix(importPath, "./") && !strings.HasPrefix(importPath, "../") {
					t.Errorf("%s has a non-relative module import %q", name, importPath)
					continue
				}
				resolved := path.Join(path.Dir(name), importPath)
				if !strings.HasPrefix(resolved, "static/assets/") || path.Ext(resolved) != ".js" {
					t.Errorf("%s imports outside the embedded modules: %q", name, importPath)
					continue
				}
				if _, readErr := staticFiles.ReadFile(resolved); readErr != nil {
					t.Errorf("%s import %q does not resolve: %v", name, importPath, readErr)
				}
			}
		}
		for _, match := range htmlAssets.FindAllStringSubmatch(text, -1) {
			if _, readErr := staticFiles.ReadFile("static" + match[1]); readErr != nil {
				t.Errorf("%s references missing asset %q: %v", name, match[1], readErr)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Sum independently compressed files, just as the asset handler serves them.
	// This intentionally does not use one artificially efficient concatenated gzip.
	for label, size := range map[string]int{
		"JS": gzipByExtension[".js"], "CSS": gzipByExtension[".css"],
		"HTML": gzipByExtension[".html"], "icons": iconsGzip, "total": totalGzip,
	} {
		budget := map[string]int{"JS": 90000, "CSS": 30000, "HTML": 5000, "icons": 10000, "total": 135000}[label]
		if size > budget {
			t.Errorf("%s gzip budget exceeded: %d bytes > %d bytes", label, size, budget)
		}
		t.Logf("%s: %d / %d gzip bytes", label, size, budget)
	}
	if totalRaw > 450000 {
		t.Errorf("uncompressed sanity cap exceeded: %d bytes > 450000 bytes", totalRaw)
	}
	t.Logf("total: %d / 450000 uncompressed bytes", totalRaw)
}

func assertDashboardHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	for name, want := range map[string]string{
		"Content-Security-Policy": "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'; require-trusted-types-for 'script'",
		"X-Content-Type-Options":  "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer",
		"Permissions-Policy": "camera=(), microphone=(), geolocation=()", "Cross-Origin-Opener-Policy": "same-origin",
	} {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}
