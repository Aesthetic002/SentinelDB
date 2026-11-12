// sentinel-certs creates everything a cluster needs to authenticate: a CA, a certificate
// per node, a client certificate, and client tokens at each role.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"sentineldb/internal/transport"
)

func main() {
	dir := flag.String("dir", "certs", "output directory")
	nodes := flag.String("nodes", "n1,n2,n3,n4,n5", "comma-separated node IDs")
	hosts := flag.String("hosts", "", "extra hostnames or IPs to add to node certificates")
	flag.Parse()
	if err := run(*dir, strings.Split(*nodes, ","), splitNonEmpty(*hosts)); err != nil {
		log.Fatal(err)
	}
}

func splitNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func run(dir string, nodes, hosts []string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.key")); err == nil {
		return fmt.Errorf("%s already holds a CA; refusing to overwrite it, because that would "+
			"lock every existing node out. Choose another directory or delete it first", dir)
	}
	ca, key, err := transport.GenerateCA(dir)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if err := transport.GenerateNodeCert(dir, n, ca, key, hosts); err != nil {
			return err
		}
	}
	if err := transport.GenerateClientCert(dir, "client", ca, key); err != nil {
		return err
	}

	var entries []transport.TokenEntry
	plain := map[string]string{}
	for _, role := range []string{"read", "write", "admin"} {
		tok, err := transport.NewToken()
		if err != nil {
			return err
		}
		plain[role] = tok
		entries = append(entries, transport.TokenEntry{Name: role, Role: role, SHA256: transport.HashToken(tok)})
	}
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "tokens.json"), b, 0o600); err != nil {
		return err
	}
	b, err = json.MarshalIndent(plain, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "client-tokens.json"), b, 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote CA, %d node certificates, a client certificate and tokens to %s\n", len(nodes), dir)
	fmt.Printf("nodes read tokens.json (hashes only); keep client-tokens.json private\n")
	return nil
}
