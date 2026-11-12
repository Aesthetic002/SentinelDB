// sentinel-node runs one SentinelDB node.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"sentineldb/internal/raft"
	"sentineldb/internal/server"
	"sentineldb/internal/transport"
)

func main() {
	var (
		id         = flag.String("id", "", "this node's ID, matching its certificate (required)")
		cluster    = flag.String("cluster", "", "all nodes, as n1=host:port,n2=host:port (peer addresses)")
		clientAddr = flag.String("client-addr", "127.0.0.1:7100", "client listen address")
		dataDir    = flag.String("data", "", "data directory (default data/<id>)")
		certDir    = flag.String("certs", "certs", "directory written by sentinel-certs")
		join       = flag.Bool("join", false, "join a running cluster: -cluster lists whom to contact")
		tick       = flag.Duration("tick", 10*time.Millisecond, "logical tick length")
		election   = flag.Int("election-ticks", 50, "base election timeout, in ticks")
		heartbeat  = flag.Int("heartbeat-ticks", 5, "heartbeat interval, in ticks")
		snapEvery  = flag.Uint64("snapshot-every", 10000, "compact the log after this many entries")
		clientCert = flag.Bool("require-client-cert", false, "require mutual TLS from clients as well as a token")
		chaos      = flag.Bool("enable-chaos", false, "allow fault injection (experiments only)")
		noSync     = flag.Bool("no-sync", false, "skip fsync: benchmarks only, voids durability")
		predict    = flag.String("predict", "", "failure-prediction model JSON; empty keeps Raft purely reactive")
		threshold  = flag.Float64("predict-threshold", 0, "override the model's handover probability threshold")
		telemetry  = flag.String("telemetry", "", "append per-window features to this CSV (data collection)")
	)
	flag.Parse()
	if *id == "" || *cluster == "" {
		fmt.Fprintln(os.Stderr, "-id and -cluster are required")
		flag.Usage()
		os.Exit(2)
	}
	all, err := parseCluster(*cluster)
	if err != nil {
		log.Fatal(err)
	}
	self := raft.NodeID(*id)

	cfg := server.Config{
		ID: self, ClientAddr: *clientAddr, DataDir: *dataDir,
		TickInterval: *tick, ElectionTicks: *election, HeartbeatTicks: *heartbeat,
		SnapshotEvery: *snapEvery, RequireClientCert: *clientCert, EnableChaos: *chaos, NoSync: *noSync,
	}
	if cfg.DataDir == "" {
		cfg.DataDir = filepath.Join("data", *id)
	}
	if addr, ok := all[self]; ok {
		cfg.PeerAddr = addr
	} else {
		log.Fatalf("-cluster has no address for this node (%s)", self)
	}
	if *join {
		cfg.Contacts = all
	} else {
		cfg.Members = all
	}

	cfg.Material, err = transport.LoadMaterial(filepath.Join(*certDir, "ca.crt"),
		filepath.Join(*certDir, *id+".crt"), filepath.Join(*certDir, *id+".key"))
	if err != nil {
		log.Fatalf("loading TLS material: %v", err)
	}
	cfg.Tokens, err = transport.LoadTokens(filepath.Join(*certDir, "tokens.json"))
	if err != nil {
		log.Fatalf("loading tokens: %v", err)
	}
	if err := configureMonitor(&cfg, *predict, *threshold, *telemetry); err != nil {
		log.Fatal(err)
	}

	s, err := server.Start(cfg)
	if err != nil {
		log.Fatalf("starting node: %v", err)
	}
	log.Printf("%s: serving clients on %s, peers on %s", self, s.ClientAddr(), s.PeerAddr())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Printf("%s: shutting down", self)
	s.Close()
}

func parseCluster(spec string) (raft.Membership, error) {
	m := raft.Membership{}
	for _, part := range strings.Split(spec, ",") {
		id, addr, ok := strings.Cut(part, "=")
		if !ok || id == "" || addr == "" {
			return nil, fmt.Errorf("bad -cluster entry %q: expected id=host:port", part)
		}
		m[raft.NodeID(id)] = addr
	}
	return m, nil
}
