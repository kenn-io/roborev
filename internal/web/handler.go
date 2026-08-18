package web

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"html"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

const ContentSecurityPolicy = "default-src 'self'; base-uri 'self'; object-src 'none'; frame-ancestors 'none'; form-action 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; style-src-attr 'unsafe-inline'; style-src-elem 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'"

const (
	browserBasePathMarker = `<meta name="roborev-base-path" content="" />`
	browserBaseHrefMarker = `<base href="/" />`
)

type httpHandler = http.Handler

type handler struct {
	files    fs.FS
	catalog  *assetCatalog
	index    []byte
	basePath string
	gzip     sync.Map
}

func NewHandler(files fs.FS, basePath string) (http.Handler, error) {
	catalog, err := loadDistribution(files)
	if err != nil {
		return nil, err
	}
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		return nil, err
	}
	if !catalog.stub {
		index, err = renderIndex(index, basePath)
		if err != nil {
			return nil, err
		}
	}
	return &handler{files: files, catalog: catalog, index: index, basePath: basePath}, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w.Header())
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == "/" {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Location", joinBasePath(h.basePath, "/reviews"))
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}

	assetPath, valid := requestAssetPath(r.URL.Path)
	if valid {
		if data, err := fs.ReadFile(h.files, assetPath); err == nil {
			contentType, known := fixedContentType(assetPath)
			if !known {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", contentType)
			if len(data) >= 1024 && compressibleContentType(contentType) {
				w.Header().Add("Vary", "Accept-Encoding")
				if acceptsGzip(r) {
					compressed, compressErr := h.gzipAsset(assetPath, data)
					if compressErr == nil {
						data = compressed
						w.Header().Set("Content-Encoding", "gzip")
					}
				}
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			if _, immutable := h.catalog.immutable[assetPath]; immutable {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodGet {
				_, _ = w.Write(data)
			}
			return
		}
	}

	if valid && acceptsHTML(r) && isNavigationPath(r.URL.Path) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(h.index)
		}
		return
	}
	http.NotFound(w, r)
}

func renderIndex(index []byte, basePath string) ([]byte, error) {
	basePathHTML := html.EscapeString(basePath)
	baseHrefHTML := html.EscapeString(joinBasePath(basePath, "/"))
	rendered, err := replaceIndexMarker(
		string(index),
		browserBasePathMarker,
		`<meta name="roborev-base-path" content="`+basePathHTML+`" />`,
		"browser base path marker",
	)
	if err != nil {
		return nil, err
	}
	rendered, err = replaceIndexMarker(
		rendered,
		browserBaseHrefMarker,
		`<base href="`+baseHrefHTML+`" />`,
		"browser base href marker",
	)
	if err != nil {
		return nil, err
	}
	return []byte(rendered), nil
}

func replaceIndexMarker(index, marker, replacement, name string) (string, error) {
	count := strings.Count(index, marker)
	if count != 1 {
		return "", fmt.Errorf("%s must occur exactly once, found %d", name, count)
	}
	return strings.Replace(index, marker, replacement, 1), nil
}

func joinBasePath(basePath, internalPath string) string {
	if basePath == "" {
		return internalPath
	}
	if internalPath == "/" {
		return basePath + "/"
	}
	return basePath + "/" + strings.TrimPrefix(internalPath, "/")
}

func (h *handler) gzipAsset(assetPath string, data []byte) ([]byte, error) {
	if cached, found := h.gzip.Load(assetPath); found {
		return cached.([]byte), nil
	}
	var compressed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(data); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	encoded := compressed.Bytes()
	h.gzip.Store(assetPath, encoded)
	return encoded, nil
}

func acceptsGzip(r *http.Request) bool {
	for value := range strings.SplitSeq(strings.ToLower(r.Header.Get("Accept-Encoding")), ",") {
		parts := strings.Split(strings.TrimSpace(value), ";")
		if parts[0] != "gzip" {
			continue
		}
		quality := 1.0
		for _, parameter := range parts[1:] {
			name, rawQuality, found := strings.Cut(strings.TrimSpace(parameter), "=")
			if name != "q" || !found {
				continue
			}
			parsed, err := strconv.ParseFloat(rawQuality, 64)
			if err != nil {
				return false
			}
			quality = parsed
		}
		return quality > 0
	}
	return false
}

func compressibleContentType(contentType string) bool {
	return strings.HasPrefix(contentType, "text/") ||
		contentType == "application/json" ||
		contentType == "application/wasm" ||
		contentType == "image/svg+xml"
}

func setSecurityHeaders(header http.Header) {
	header.Set("Content-Security-Policy", ContentSecurityPolicy)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
}

func requestAssetPath(requestPath string) (string, bool) {
	if requestPath == "" || !strings.HasPrefix(requestPath, "/") || strings.ContainsAny(requestPath, "\\\x00") {
		return "", false
	}
	assetPath := strings.TrimPrefix(requestPath, "/")
	if assetPath == "" || !fs.ValidPath(assetPath) {
		return "", false
	}
	for segment := range strings.SplitSeq(assetPath, "/") {
		if segment == "." || segment == ".." || strings.HasPrefix(segment, ".") {
			return "", false
		}
	}
	return assetPath, true
}

func acceptsHTML(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")
}

func isNavigationPath(requestPath string) bool {
	if requestPath != "/reviews" && !strings.HasPrefix(requestPath, "/reviews/") && requestPath != "/analytics" {
		return false
	}
	for segment := range strings.SplitSeq(strings.TrimPrefix(requestPath, "/"), "/") {
		if strings.Contains(segment, ".") {
			return false
		}
	}
	return true
}

func fixedContentType(assetPath string) (string, bool) {
	extensions := map[string]string{
		".css":   "text/css; charset=utf-8",
		".html":  "text/html; charset=utf-8",
		".js":    "text/javascript; charset=utf-8",
		".json":  "application/json; charset=utf-8",
		".png":   "image/png",
		".svg":   "image/svg+xml",
		".wasm":  "application/wasm",
		".woff2": "font/woff2",
	}
	for extension, contentType := range extensions {
		if strings.HasSuffix(strings.ToLower(assetPath), extension) {
			return contentType, true
		}
	}
	return "", false
}
