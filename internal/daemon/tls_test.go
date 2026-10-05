package daemon

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

// newTestPKI writes a CA, a daemon certificate for 127.0.0.1, and a client
// certificate, the way an operator would before enabling [daemon_tls].
func newTestPKI(t *testing.T) config.DaemonTLSConfig {
	t.Helper()
	dir := t.TempDir()
	writePEM := func(name, blockType string, der []byte) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600))
		return path
	}
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	issue := func(name string, serial int64, usage x509.ExtKeyUsage) (string, string) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		template := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: name},
			NotBefore:    now.Add(-time.Hour),
			NotAfter:     now.Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{usage},
			IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		}
		der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
		require.NoError(t, err)
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		require.NoError(t, err)
		return writePEM(name+".pem", "CERTIFICATE", der), writePEM(name+"-key.pem", "PRIVATE KEY", keyDER)
	}
	certFile, keyFile := issue("daemon", 2, x509.ExtKeyUsageServerAuth)
	clientCertFile, clientKeyFile := issue("client", 3, x509.ExtKeyUsageClientAuth)
	return config.DaemonTLSConfig{
		CAFile:         writePEM("ca.pem", "CERTIFICATE", caDER),
		CertFile:       certFile,
		KeyFile:        keyFile,
		ClientCertFile: clientCertFile,
		ClientKeyFile:  clientKeyFile,
	}
}

func writeMutualTLSClientConfig(t *testing.T, key string, settings config.DaemonTLSConfig) {
	t.Helper()
	contents := fmt.Sprintf(
		"auth_key = %q\n[daemon_tls]\nca_file = %q\nclient_cert_file = %q\nclient_key_file = %q\n",
		key, settings.CAFile, settings.ClientCertFile, settings.ClientKeyFile,
	)
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(contents), 0o600))
}

func TestAuthMutualTLSOnTCPListener(t *testing.T) {
	testenv.SetDataDir(t)
	setShortRuntimeDir(t)
	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	daemonPKI := newTestPKI(t)
	writeMutualTLSClientConfig(t, key, daemonPKI)
	db, _ := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	cfg.AuthKey = key
	cfg.DaemonTLS = daemonPKI
	cfg.ServerAddr = "127.0.0.1:0"
	cfg.Web.Enabled = false
	server := NewServer(db, cfg, "")
	errCh, info := startServerAndWaitForRuntime(t, server)
	t.Cleanup(func() { stopTestServer(t, server, errCh) })
	tcp := info.Endpoints()[0]
	require.Equal(t, "tcp", tcp.Network)

	ping, err := ProbeDaemon(tcp, time.Second)
	require.NoError(t, err, "a client with the configured certificates sends the key over TCP")
	assert.True(t, ping.OK)

	t.Run("client trusting another CA", func(t *testing.T) {
		writeMutualTLSClientConfig(t, key, newTestPKI(t))
		_, err := ProbeDaemon(tcp, time.Second)
		var verifyErr *tls.CertificateVerificationError
		require.ErrorAs(t, err, &verifyErr)
	})

	t.Run("client without a certificate", func(t *testing.T) {
		pool := x509.NewCertPool()
		caPEM, err := os.ReadFile(daemonPKI.CAFile)
		require.NoError(t, err)
		require.True(t, pool.AppendCertsFromPEM(caPEM))
		client := &http.Client{Timeout: time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		}}
		resp, err := client.Get("https://" + tcp.Address + "/api/ping")
		if resp != nil {
			resp.Body.Close()
		}
		require.Error(t, err)
	})
}
