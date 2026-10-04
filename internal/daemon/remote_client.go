package daemon

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"time"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/requestsigning"
)

func (e DaemonEndpoint) IsRemote() bool { return e.Network == "https" }
func (e DaemonEndpoint) remoteHTTPClient(timeout time.Duration) *http.Client {
	if e.accessErr != nil {
		return remoteErrorClient(timeout, e.accessErr)
	}
	cfg, err := config.LoadRemoteClient()
	if err != nil {
		return remoteErrorClient(timeout, err)
	}
	if err = cfg.MatchOrigin(e.BaseURL()); err != nil {
		return remoteErrorClient(timeout, err)
	}
	if _, err = cfg.SigningKey(); err != nil {
		return remoteErrorClient(timeout, err)
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true}
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	}
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CAFile != "" {
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		data, err := os.ReadFile(cfg.CAFile)
		if err != nil || !roots.AppendCertsFromPEM(data) {
			return remoteErrorClient(timeout, errors.New("cannot load remote HTTPS CA file"))
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	client, err := requestsigning.HTTPClient(e.BaseURL(), &http.Client{Transport: transport, Timeout: timeout}, func() (requestsigning.Key, error) {
		cfg, err := config.LoadRemoteClient()
		if err != nil {
			return requestsigning.Key{}, err
		}
		if err = cfg.MatchOrigin(e.BaseURL()); err != nil {
			return requestsigning.Key{}, err
		}
		return cfg.SigningKey()
	})
	if err != nil {
		return remoteErrorClient(timeout, err)
	}
	// The signing key is the only remote credential; the daemon auth_key never
	// leaves the server machine.
	return client
}

func remoteErrorClient(timeout time.Duration, err error) *http.Client {
	return &http.Client{Timeout: timeout, Transport: remoteClientError{err}}
}

type remoteClientError struct{ err error }

func (t remoteClientError) RoundTrip(*http.Request) (*http.Response, error) { return nil, t.err }
