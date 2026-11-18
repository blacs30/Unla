package mcpproxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/amoylab/unla/internal/common/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) testCA {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	return testCA{
		cert: cert,
		key:  key,
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

func (ca testCA) issue(t *testing.T, cn string, server bool, ips []net.IP) (certPEM, keyPEM []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	usage := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if server {
		usage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  usage,
		IPAddresses:  ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	require.NoError(t, err)

	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

// TestStreamableTransport_MutualTLS verifies that the gateway presents a client
// certificate and verifies the server against the provided CA.
func TestStreamableTransport_MutualTLS(t *testing.T) {
	ca := newTestCA(t)

	serverCertPEM, serverKeyPEM := ca.issue(t, "127.0.0.1", true, []net.IP{net.ParseIP("127.0.0.1")})
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	require.NoError(t, err)

	clientCertPEM, clientKeyPEM := ca.issue(t, "unla", false, nil)

	clientCAs := x509.NewCertPool()
	require.True(t, clientCAs.AppendCertsFromPEM(ca.pem))

	var clientCN string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			clientCN = r.TLS.PeerCertificates[0].Subject.CommonName
		}

		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID *int `json:"id"`
		}
		_ = json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "application/json")
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      *req.ID,
			"result": map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "mtls", "version": "1"},
			},
		})
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
	}
	srv.StartTLS()
	defer srv.Close()

	dir := t.TempDir()
	cfg := config.MCPServerConfig{
		Type: "streamable-http",
		Name: "paperless",
		URL:  srv.URL,
		TLS: &config.MCPTLSConfig{
			CAFile:   writeFile(t, dir, "ca.pem", ca.pem),
			CertFile: writeFile(t, dir, "client.pem", clientCertPEM),
			KeyFile:  writeFile(t, dir, "client-key.pem", clientKeyPEM),
		},
	}

	tr := &StreamableTransport{cfg: cfg}
	require.NoError(t, tr.Start(context.Background(), nil))
	defer func() { _ = tr.Stop(context.Background()) }()

	assert.Equal(t, "unla", clientCN, "server should have seen the gateway client certificate")
}

// TestStreamableTransport_MutualTLSRejectsMissingClientCert ensures the test
// setup really enforces client certificates (i.e. the positive test is meaningful).
func TestStreamableTransport_MutualTLSRejectsMissingClientCert(t *testing.T) {
	ca := newTestCA(t)
	serverCertPEM, serverKeyPEM := ca.issue(t, "127.0.0.1", true, []net.IP{net.ParseIP("127.0.0.1")})
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	require.NoError(t, err)

	clientCAs := x509.NewCertPool()
	require.True(t, clientCAs.AppendCertsFromPEM(ca.pem))

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
	}
	srv.StartTLS()
	defer srv.Close()

	dir := t.TempDir()
	tr := &StreamableTransport{cfg: config.MCPServerConfig{
		Type: "streamable-http",
		Name: "paperless",
		URL:  srv.URL,
		TLS:  &config.MCPTLSConfig{CAFile: writeFile(t, dir, "ca.pem", ca.pem)},
	}}

	err = tr.Start(context.Background(), nil)
	require.Error(t, err)
	assert.False(t, tr.IsRunning())
}
