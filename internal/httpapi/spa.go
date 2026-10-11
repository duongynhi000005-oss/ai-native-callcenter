// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// compressibleExt lists the extensions the web build pre-compresses to a .gz
// sibling (web/vite.config.ts, precompressDist). Keep the two lists in step.
var compressibleExt = map[string]bool{
	".js": true, ".css": true, ".svg": true, ".json": true, ".html": true,
}

// SPAHandler serves the built single-page application from an fs.FS.
//
// Hashed assets under /assets are immutable and cached aggressively; every
// other path falls back to index.html so client-side routing works on reload.
//
// The build writes a .gz beside each compressible file. When the client accepts
// gzip, the sibling is served as is with Content-Encoding: gzip and the
// original's Content-Type; nothing is ever compressed at request time.
func SPAHandler(dist fs.FS) http.Handler {
	files := http.FileServer(http.FS(dist))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if upath == "" {
			upath = "index.html"
		}

		if f, err := dist.Open(upath); err == nil {
			f.Close()
			if strings.HasPrefix(upath, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			if upath == "index.html" {
				// The shell names the hashed assets, so it must always be revalidated.
				w.Header().Set("Cache-Control", "no-cache")
			}
			if compressibleExt[path.Ext(upath)] {
				w.Header().Add("Vary", "Accept-Encoding")
				// "/index.html" is left to FileServer, which redirects it to "/".
				if (upath != "index.html" || r.URL.Path == "/") && serveGzipSibling(w, r, dist, upath) {
					return
				}
			}
			files.ServeHTTP(w, r)
			return
		}

		// Unknown path: hand the route to the SPA router.
		index, err := dist.Open("index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		index.Close()
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Add("Vary", "Accept-Encoding")
		if serveGzipSibling(w, r, dist, "index.html") {
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		files.ServeHTTP(w, r2)
	})
}

// serveGzipSibling serves name+".gz" when the request accepts gzip and the
// sibling exists. It reports whether it wrote the response.
func serveGzipSibling(w http.ResponseWriter, r *http.Request, dist fs.FS, name string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
		return false
	}
	gz, err := dist.Open(name + ".gz")
	if err != nil {
		return false
	}
	defer gz.Close()
	info, err := gz.Stat()
	rs, ok := gz.(io.ReadSeeker)
	if err != nil || info.IsDir() || !ok {
		return false
	}
	// Without an explicit type ServeContent would label the body by the .gz name.
	ct := mime.TypeByExtension(path.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Encoding", "gzip")
	http.ServeContent(w, r, name, info.ModTime(), rs)
	return true
}

// acceptsGzip reports whether an Accept-Encoding header permits a gzip body:
// an explicit gzip token decides by its q-value; otherwise a "*" with q>0
// allows it. Tokens are case-insensitive and whitespace is ignored.
func acceptsGzip(header string) bool {
	gzipQ, starQ := -1.0, -1.0
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(part, ";")
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "gzip" && name != "*" {
			continue
		}
		q := 1.0
		for _, p := range strings.Split(params, ";") {
			k, v, ok := strings.Cut(p, "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					q = f
				} else {
					q = 0
				}
			}
		}
		if name == "gzip" {
			gzipQ = q
		} else {
			starQ = q
		}
	}
	if gzipQ >= 0 {
		return gzipQ > 0
	}
	return starQ > 0
}
