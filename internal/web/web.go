// Package web serves the Conductor dashboard.
//
// The dashboard is a self-contained single-page application with no build step and no
// external requests. That is partly convenience — `conductord` is one binary — and partly
// the point: a coordination dashboard that phones out to a CDN is a coordination dashboard
// that leaks which repository your team is working on. TestNoExternalRequests holds that
// property mechanically.
package web

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed static
var staticFS embed.FS

// Dashboard returns the SPA's index page, for callers that only want the bytes.
func Dashboard() []byte {
	body, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		return nil
	}
	return body
}

// Handler serves the dashboard.
//
// Assets live under /static/ and are cacheable; every other path returns the index page so
// client-side routes (/tasks/T-42, /usage) deep-link and survive a reload. The index is
// never cached: it is the one file whose freshness decides whether a redeploy is visible.
func Handler() http.Handler {
	index := Dashboard()
	assets, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if rel, ok := strings.CutPrefix(r.URL.Path, "/static/"); ok {
			serveAsset(w, r, assets, rel)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(index)
		}
	})
}

func serveAsset(w http.ResponseWriter, r *http.Request, assets fs.FS, rel string) {
	clean := path.Clean("/" + rel)[1:]
	if clean == "" || clean == "index.html" || strings.HasSuffix(clean, "/") {
		http.NotFound(w, r)
		return
	}
	body, err := fs.ReadFile(assets, clean)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType(clean))
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}

// contentType maps an asset's extension to a MIME type. The common ones are pinned rather
// than left to the platform's mime table, which on some systems reports .js as text/plain —
// and a module script served as text/plain is refused by every browser.
func contentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json", ".map":
		return "application/json; charset=utf-8"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	case ".txt", ".md":
		return "text/plain; charset=utf-8"
	}
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}
