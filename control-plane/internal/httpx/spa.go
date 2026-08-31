package httpx

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	cacheImmutable = "public, max-age=31536000, immutable"
	// index.html must revalidate every load, or a new deploy's asset hashes need
	// a hard refresh to appear.
	cacheNoCache = "no-cache"
)

// SPAHandler serves the built React SPA from webRoot.
//
// An /assets/* miss must 404, never fall through to the SPA HTML: that
// MIME-blocks the module script and yields a blank page with no console error.
func SPAHandler(webRoot string) http.Handler {
	fs := http.FileServer(http.Dir(webRoot))
	index := filepath.Join(webRoot, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cleanPath := filepath.Clean("/" + r.URL.Path)

		// Unmatched /v1/* must 404, never fall through to the SPA: ServeMux routes
		// every unregistered /v1 path to this catch-all, so index.html would answer
		// 200 + HTML and make a missing route look like a successful call.
		if strings.HasPrefix(cleanPath, "/v1/") {
			WriteError(w, http.StatusNotFound, CodeNotFound, "not found")
			return
		}

		if strings.HasPrefix(cleanPath, "/assets/") {
			candidate := filepath.Join(webRoot, cleanPath)
			if _, err := os.Stat(candidate); err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Cache-Control", cacheImmutable)
			fs.ServeHTTP(w, r)
			return
		}

		candidate := filepath.Join(webRoot, cleanPath)
		if fi, err := os.Stat(candidate); err != nil || fi.IsDir() {
			w.Header().Set("Cache-Control", cacheNoCache)
			http.ServeFile(w, r, index)
			return
		}
		fs.ServeHTTP(w, r)
	})
}
