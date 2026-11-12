// Package transport moves Raft messages between nodes over mutually authenticated TLS.
package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"sentineldb/internal/protocol"
)

const (
	// NodeOrg marks a certificate as belonging to a cluster node. A client certificate
	// signed by the same CA lacks it, so it cannot pose as a node on the peer port.
	NodeOrg = "sentineldb-node"
	// ServerName is a SAN on every node certificate. Clients verify it, proving they
	// reached some node of this cluster without having to know each node's address.
	ServerName = "sentineldb-node"
)

// Material is the TLS identity of a process.
type Material struct {
	Cert tls.Certificate
	CA   *x509.CertPool
}

// LoadMaterial reads a CA certificate and a key pair from PEM files.
func LoadMaterial(caFile, certFile, keyFile string) (*Material, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("%s holds no usable CA certificate", caFile)
	}
	m := &Material{CA: pool}
	if certFile != "" {
		m.Cert, err = tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, err
		}
	}
	return m, nil
}

// PeerServerConfig is for the peer listener: the other side must present a node cert.
func (m *Material) PeerServerConfig() *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{m.Cert},
		ClientCAs:    m.CA,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !isNodeCert(cs.PeerCertificates[0]) {
				return errors.New("peer certificate is not a node certificate")
			}
			return nil
		},
	}
}

// PeerClientConfig is for dialing a peer. Using the peer's ID as the server name means
// a certificate for some other node is refused: only n2 can answer as n2.
func (m *Material) PeerClientConfig(peerID string) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{m.Cert},
		RootCAs:      m.CA,
		ServerName:   peerID,
	}
}

// ClientServerConfig is for the client listener. Clients always verify the server. When
// requireCert is set, clients must also present a CA-signed certificate (mutual TLS);
// either way, the token still decides what they may do.
func (m *Material) ClientServerConfig(requireCert bool) *tls.Config {
	auth := tls.VerifyClientCertIfGiven
	if requireCert {
		auth = tls.RequireAndVerifyClientCert
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{m.Cert},
		ClientCAs:    m.CA,
		ClientAuth:   auth,
	}
}

// ClientConfig is for a client dialing any node of the cluster.
func (m *Material) ClientConfig() *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: m.CA, ServerName: ServerName}
	if m.Cert.Certificate != nil {
		cfg.Certificates = []tls.Certificate{m.Cert}
	}
	return cfg
}

func isNodeCert(c *x509.Certificate) bool {
	for _, o := range c.Subject.Organization {
		if o == NodeOrg {
			return true
		}
	}
	return false
}

// PeerIdentity returns the node ID proven by a verified peer connection.
func PeerIdentity(conn *tls.Conn) (string, error) {
	cs := conn.ConnectionState()
	if len(cs.PeerCertificates) == 0 {
		return "", errors.New("connection has no peer certificate")
	}
	return cs.PeerCertificates[0].Subject.CommonName, nil
}

// --- certificate generation --------------------------------------------------

func newKey() (*ecdsa.PrivateKey, error) { return ecdsa.GenerateKey(elliptic.P256(), rand.Reader) }

func writePEM(path, kind string, der []byte, mode os.FileMode) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), mode)
}

func writeKey(path string, k *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return err
	}
	return writePEM(path, "EC PRIVATE KEY", der, 0o600)
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

// GenerateCA creates a CA in dir and returns it for signing.
func GenerateCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := newKey()
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: "SentinelDB cluster CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(5, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	if err := writePEM(filepath.Join(dir, "ca.crt"), "CERTIFICATE", der, 0o644); err != nil {
		return nil, nil, err
	}
	if err := writeKey(filepath.Join(dir, "ca.key"), key); err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

// GenerateNodeCert issues a node certificate, writing <id>.crt and <id>.key.
func GenerateNodeCert(dir, id string, ca *x509.Certificate, caKey *ecdsa.PrivateKey, hosts []string) error {
	sn, err := serial()
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: id, Organization: []string{NodeOrg}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(2, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{id, ServerName, "localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	return issue(dir, id, tmpl, ca, caKey)
}

// GenerateClientCert issues a certificate for a client. It carries no node marking.
func GenerateClientCert(dir, name string, ca *x509.Certificate, caKey *ecdsa.PrivateKey) error {
	sn, err := serial()
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(2, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	return issue(dir, name, tmpl, ca, caKey)
}

func issue(dir, name string, tmpl, ca *x509.Certificate, caKey *ecdsa.PrivateKey) error {
	key, err := newKey()
	if err != nil {
		return err
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	if err := writePEM(filepath.Join(dir, name+".crt"), "CERTIFICATE", der, 0o644); err != nil {
		return err
	}
	return writeKey(filepath.Join(dir, name+".key"), key)
}

// --- tokens --------------------------------------------------------------------

// TokenEntry is one line of the tokens file. Only a hash is stored, so reading the file
// on a node does not reveal any token.
type TokenEntry struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	SHA256 string `json:"sha256"`
}

// HashToken returns the stored form of a token.
func HashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// NewToken generates a random 256-bit token.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Tokens maps token hashes to roles.
type Tokens map[string]protocol.Role

// LoadTokens reads a tokens file.
func LoadTokens(path string) (Tokens, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []TokenEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	t := Tokens{}
	for _, e := range entries {
		r, err := protocol.ParseRole(e.Role)
		if err != nil {
			return nil, fmt.Errorf("%s: token %s: %w", path, e.Name, err)
		}
		t[e.SHA256] = r
	}
	return t, nil
}

// Check returns a token's role, or RoleNone. Lookup is by hash, so its timing depends on
// the hash and not on how much of a secret token an attacker has guessed.
func (t Tokens) Check(tok string) protocol.Role { return t[HashToken(tok)] }
