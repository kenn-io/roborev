//go:build integration

package daemon

import (
	"context"
	"net/http/httptest"
)

// NewRemoteTestServer serves the remote API handler on a loopback test
// server and identifies every caller as a tailnet node with the given
// access. It is built only with the integration tag, so integration tests in
// other packages can drive the real handler with the real client without a
// production export.
func (s *Server) NewRemoteTestServer(access RemoteAccess) *httptest.Server {
	whois := func(context.Context, string) (RemoteCaller, error) {
		return RemoteCaller{Node: "client.example-tailnet.ts.net.", Login: "user-a@example.com", Access: access}, nil
	}
	ts := httptest.NewUnstartedServer(s.newRemoteHandler(s.httpServer.Handler, whois))
	ts.Config.ConnContext = remoteConnContext
	ts.Start()
	return ts
}
