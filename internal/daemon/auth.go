package daemon

import (
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"strings"

	"go.kenn.io/roborev/internal/auth"
)

// withAuthentication captures the startup key so a config reload or deletion
// cannot weaken authentication on an already running daemon.
func withAuthentication(next http.Handler, key string) http.Handler {
	if key == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the browser session boundary can set this internal principal.
		if _, authenticated := BrowserPrincipalFromContext(r.Context()); authenticated {
			next.ServeHTTP(w, r)
			return
		}
		headers := r.Header.Values("Authorization")
		authorized := false
		if len(headers) == 1 {
			scheme, credential, ok := strings.Cut(headers[0], " ")
			authorized = ok && strings.EqualFold(scheme, "Bearer") && auth.EqualKey(key, credential)
		}
		if !authorized {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("WWW-Authenticate", "Bearer")
			if r.Method == http.MethodGet && r.URL.Path == "/api/ping" {
				nonces := r.Header.Values("X-Roborev-Auth-Nonce")
				if len(nonces) == 1 {
					nonce, err := hex.DecodeString(nonces[0])
					if err == nil && len(nonce) != 0 {
						w.Header().Set("X-Roborev-Auth-Proof", hex.EncodeToString(auth.ServerProof(key, nonce)))
					}
				}
			}
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.MarshalWrite(w, ErrorResponse{Error: "daemon authentication required"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
