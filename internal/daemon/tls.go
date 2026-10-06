package daemon

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"go.kenn.io/roborev/internal/config"
)

// serverTLSConfig requires every TCP client to present a certificate signed
// by the configured CA.
func serverTLSConfig(c config.DaemonTLSConfig) (*tls.Config, error) {
	if c.CertFile == "" || c.KeyFile == "" {
		return nil, errors.New("daemon_tls.cert_file and key_file are required for the daemon")
	}
	certificate, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load daemon_tls server certificate: %w", err)
	}
	pool, err := loadCAPool(c.CAFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// clientTLSConfig trusts only the configured CA and presents the client
// certificate.
func clientTLSConfig(c config.DaemonTLSConfig) (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(c.ClientCertFile, c.ClientKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load daemon_tls client certificate: %w", err)
	}
	pool, err := loadCAPool(c.CAFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{certificate},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func loadCAPool(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read daemon_tls.ca_file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, errors.New("daemon_tls.ca_file contains no PEM certificates")
	}
	return pool, nil
}

// CheckTLSFiles loads the certificate files [daemon_tls] names, so a missing
// or mismatched file is reported before the daemon or a command fails on it.
func CheckTLSFiles(c config.DaemonTLSConfig) error {
	if !c.Enabled() {
		return nil
	}
	if _, err := clientTLSConfig(c); err != nil {
		return err
	}
	if c.CertFile != "" {
		if _, err := serverTLSConfig(c); err != nil {
			return err
		}
	}
	return nil
}
