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
	"time"
)

func newDaemonTLSCertificate(address string) (tls.Certificate, string, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("parse daemon TLS address: %w", err)
	}
	ipAddresses := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return tls.Certificate{}, "", fmt.Errorf("daemon TLS address host %q is not loopback", host)
		}
		if !ip.Equal(ipAddresses[0]) && !ip.Equal(ipAddresses[1]) {
			ipAddresses = append(ipAddresses, ip)
		}
	} else if host != "localhost" {
		return tls.Certificate{}, "", fmt.Errorf("daemon TLS address host %q is not localhost or a loopback IP", host)
	}

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial.SetBit(serial, 0, 1)
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "roborev daemon"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.AddDate(100, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           ipAddresses,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER})
	certificate, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return certificate, string(certificatePEM), nil
}
