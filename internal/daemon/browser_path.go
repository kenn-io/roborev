package daemon

import (
	"fmt"
	"strings"
)

// normalizeBrowserPath maps an externally mounted browser path to the
// internal path understood by the daemon. The redirect result is true only
// for the exact configured prefix, which must use its canonical slash form.
func normalizeBrowserPath(requestPath, basePath string) (string, bool, error) {
	if requestPath == "" {
		requestPath = "/"
	}
	if basePath == "" {
		return requestPath, false, nil
	}
	if requestPath == basePath {
		return "/", true, nil
	}
	prefix := basePath + "/"
	if !strings.HasPrefix(requestPath, prefix) {
		return "", false, fmt.Errorf("browser path is outside configured base path")
	}
	return strings.TrimPrefix(requestPath, basePath), false, nil
}

func joinBrowserPath(basePath, internalPath string) string {
	internalPath = strings.TrimLeft(internalPath, `/\`)
	if basePath == "" {
		return "/" + internalPath
	}
	if internalPath == "" {
		return basePath + "/"
	}
	return basePath + "/" + internalPath
}
