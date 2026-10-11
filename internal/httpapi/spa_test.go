// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

const (
	jsBody    = "console.log('app');// padding padding padding padding"
	cssBody   = "body{margin:0}/* padding padding padding padding */"
	indexBody = "<!doctype html><html><body>spa</body></html>"
)

func spaFS(t *testing.T) fstest.MapFS {
	return fstest.MapFS{
		"index.html":           {Data: []byte(indexBody)},
		"index.html.gz":        {Data: gz(t, indexBody)},
		"assets/app-1.js":      {Data: []byte(jsBody)},
		"assets/app-1.js.gz":   {Data: gz(t, jsBody)},
		"assets/app-1.css":     {Data: []byte(cssBody)},
		"assets/app-1.css.gz":  {Data: gz(t, cssBody)},
		"assets/lone-2.js":     {Data: []byte(jsBody)},
		"assets/logo-3.png":    {Data: []byte("PNGDATA")},
		"assets/logo-3.png.gz": {Data: []byte("must never be served")},
	}
}

func get(t *testing.T, h http.Handler, target, acceptEnc string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if acceptEnc != "" {
		req.Header.Set("Accept-Encoding", acceptEnc)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func gunzipped(t *testing.T, resp *http.Response) string {
	t.Helper()
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSPAServesPrecompressedAssetWhenGzipAccepted(t *testing.T) {
	h := SPAHandler(spaFS(t))
	for _, c := range []struct{ url, want, typ string }{
		{"/assets/app-1.js", jsBody, "javascript"},
		{"/assets/app-1.css", cssBody, "text/css"},
	} {
		resp := get(t, h, c.url, "gzip, deflate, br")
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d", c.url, resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
			t.Errorf("%s: Content-Encoding %q", c.url, got)
		}
		if got := resp.Header.Get("Content-Type"); !strings.Contains(got, c.typ) {
			t.Errorf("%s: Content-Type %q", c.url, got)
		}
		if got := resp.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Errorf("%s: Cache-Control %q", c.url, got)
		}
		if got := resp.Header.Get("Vary"); got != "Accept-Encoding" {
			t.Errorf("%s: Vary %q", c.url, got)
		}
		if got := gunzipped(t, resp); got != c.want {
			t.Errorf("%s: decoded body %q", c.url, got)
		}
	}
}

func TestSPAServesOriginalWithoutAcceptEncoding(t *testing.T) {
	resp := get(t, SPAHandler(spaFS(t)), "/assets/app-1.js", "")
	if resp.Header.Get("Content-Encoding") != "" {
		t.Errorf("Content-Encoding %q", resp.Header.Get("Content-Encoding"))
	}
	if resp.Header.Get("Vary") != "Accept-Encoding" {
		t.Errorf("Vary %q", resp.Header.Get("Vary"))
	}
	if got := bodyOf(t, resp); got != jsBody {
		t.Errorf("body %q", got)
	}
}

func TestSPAGzipQZeroIsIdentity(t *testing.T) {
	resp := get(t, SPAHandler(spaFS(t)), "/assets/app-1.js", "gzip;q=0, br")
	if resp.Header.Get("Content-Encoding") != "" {
		t.Errorf("Content-Encoding %q", resp.Header.Get("Content-Encoding"))
	}
	if got := bodyOf(t, resp); got != jsBody {
		t.Errorf("body %q", got)
	}
}

func TestSPACompressibleWithoutSiblingServesOriginalWithVary(t *testing.T) {
	resp := get(t, SPAHandler(spaFS(t)), "/assets/lone-2.js", "gzip")
	if resp.Header.Get("Content-Encoding") != "" {
		t.Errorf("Content-Encoding %q", resp.Header.Get("Content-Encoding"))
	}
	if resp.Header.Get("Vary") != "Accept-Encoding" {
		t.Errorf("Vary %q", resp.Header.Get("Vary"))
	}
	if got := bodyOf(t, resp); got != jsBody {
		t.Errorf("body %q", got)
	}
}

func TestSPANonCompressibleIsUntouched(t *testing.T) {
	resp := get(t, SPAHandler(spaFS(t)), "/assets/logo-3.png", "gzip")
	if resp.Header.Get("Content-Encoding") != "" || resp.Header.Get("Vary") != "" {
		t.Errorf("encoding %q vary %q", resp.Header.Get("Content-Encoding"), resp.Header.Get("Vary"))
	}
	if got := bodyOf(t, resp); got != "PNGDATA" {
		t.Errorf("body %q", got)
	}
}

func TestSPAIndexIsNoCacheOnBothBranches(t *testing.T) {
	h := SPAHandler(spaFS(t))
	for _, target := range []string{"/", "/some/route"} {
		for _, enc := range []string{"gzip", ""} {
			resp := get(t, h, target, enc)
			if resp.StatusCode != 200 {
				t.Fatalf("%s: status %d", target, resp.StatusCode)
			}
			if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
				t.Errorf("%s: Cache-Control %q", target, got)
			}
			if resp.Header.Get("Vary") != "Accept-Encoding" {
				t.Errorf("%s: Vary %q", target, resp.Header.Get("Vary"))
			}
			if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
				t.Errorf("%s: Content-Type %q", target, got)
			}
			var body string
			if enc == "gzip" {
				if resp.Header.Get("Content-Encoding") != "gzip" {
					t.Errorf("%s: not gzip", target)
				}
				body = gunzipped(t, resp)
			} else {
				if resp.Header.Get("Content-Encoding") != "" {
					t.Errorf("%s: unexpected encoding", target)
				}
				body = bodyOf(t, resp)
			}
			if body != indexBody {
				t.Errorf("%s: body %q", target, body)
			}
		}
	}
}

func TestSPAIndexHTMLStillRedirects(t *testing.T) {
	resp := get(t, SPAHandler(spaFS(t)), "/index.html", "gzip")
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("status %d", resp.StatusCode)
	}
}

func TestSPAMissingIndexIs404(t *testing.T) {
	resp := get(t, SPAHandler(fstest.MapFS{}), "/x", "gzip")
	if resp.StatusCode != 404 {
		t.Errorf("status %d", resp.StatusCode)
	}
}

func TestAcceptsGzip(t *testing.T) {
	for _, c := range []struct {
		header string
		want   bool
	}{
		{"", false},
		{"gzip", true},
		{"GZip", true},
		{" gzip , deflate", true},
		{"deflate, br", false},
		{"gzip;q=0", false},
		{"gzip; q=0.0", false},
		{"gzip;q=0.5", true},
		{"gzip ; Q=1", true},
		{"*", true},
		{"*;q=0", false},
		{"*;q=0.1", true},
		{"gzip;q=0, *", false},
		{"gzip, *;q=0", true},
		{"br, *", true},
		{"identity", false},
		{"gzip;q=bogus", false},
		{"x-gzip", false},
	} {
		if got := acceptsGzip(c.header); got != c.want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", c.header, got, c.want)
		}
	}
}
