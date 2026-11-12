package server

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/gob"
	"encoding/pem"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type (
	certT = *x509.Certificate
	keyT  = *ecdsa.PrivateKey
)

func readCAFiles(t *testing.T, dir string) (certT, keyT) {
	t.Helper()
	cb, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(cb)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ = pem.Decode(kb)
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func gobEncoder(w io.Writer) *gob.Encoder { return gob.NewEncoder(w) }
