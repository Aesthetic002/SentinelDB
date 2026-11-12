// sentinel-cli is an interactive SQL shell for a SentinelDB cluster.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sentineldb/internal/client"
	"sentineldb/internal/protocol"
	"sentineldb/internal/transport"
)

func main() {
	var (
		addrs   = flag.String("addrs", "127.0.0.1:7101,127.0.0.1:7102,127.0.0.1:7103,127.0.0.1:7104,127.0.0.1:7105", "node client addresses")
		certDir = flag.String("certs", "certs", "directory written by sentinel-certs")
		role    = flag.String("role", "admin", "which token to use from client-tokens.json: read, write or admin")
		token   = flag.String("token", "", "token (overrides -role)")
		useCert = flag.Bool("client-cert", false, "present certs/client.crt, for -require-client-cert nodes")
		query   = flag.String("e", "", "run one statement and exit")
		stale   = flag.Bool("stale", false, "read from the connected node's local copy (may lag the leader)")
	)
	flag.Parse()
	var cert, key string
	if *useCert {
		cert, key = filepath.Join(*certDir, "client.crt"), filepath.Join(*certDir, "client.key")
	}
	mat, err := transport.LoadMaterial(filepath.Join(*certDir, "ca.crt"), cert, key)
	if err != nil {
		log.Fatal(err)
	}
	tok := *token
	if tok == "" {
		b, err := os.ReadFile(filepath.Join(*certDir, "client-tokens.json"))
		if err != nil {
			log.Fatalf("no -token given and cannot read the tokens file: %v", err)
		}
		var toks map[string]string
		if err := json.Unmarshal(b, &toks); err != nil {
			log.Fatal(err)
		}
		tok = toks[*role]
	}
	sh := &shell{
		cl:    client.New(client.Config{Addrs: strings.Split(*addrs, ","), Token: tok, TLS: mat.ClientConfig()}),
		stale: *stale,
	}
	defer sh.cl.Close()
	if *query != "" {
		if !sh.run(*query) {
			os.Exit(1)
		}
		return
	}
	sh.repl()
}

type shell struct {
	cl    *client.Client
	tx    *client.Tx
	stale bool
}

func (s *shell) repl() {
	fmt.Println(`SentinelDB shell. End statements with ';'. Type \help for commands.`)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 64*1024), protocolMax)
	var buf strings.Builder
	for {
		prompt := "sentinel> "
		switch {
		case buf.Len() > 0:
			prompt = "      ... "
		case s.tx != nil:
			prompt = "sentinel*> "
		}
		fmt.Print(prompt)
		if !in.Scan() {
			fmt.Println()
			if s.tx != nil {
				s.tx.Rollback()
			}
			return
		}
		// PowerShell can put a byte-order mark at the start of piped input. It is not part
		// of the statement, and would otherwise fail the first one as a syntax error.
		line := strings.TrimSpace(strings.TrimPrefix(in.Text(), "\xef\xbb\xbf"))
		if buf.Len() == 0 && strings.HasPrefix(line, `\`) {
			if s.command(line) {
				return
			}
			continue
		}
		// Newlines must survive: a "--" comment on one line ends at that line.
		buf.WriteString(line)
		buf.WriteByte('\n')
		if strings.HasSuffix(line, ";") {
			s.run(strings.TrimSpace(buf.String()))
			buf.Reset()
		}
	}
}

const protocolMax = 4 << 20

func (s *shell) command(line string) (quit bool) {
	f := strings.Fields(line)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch f[0] {
	case `\q`, `\quit`:
		return true
	case `\help`:
		fmt.Println(`  \status              this node's view of the cluster
  \transfer <node>     hand leadership to a node (connect to the leader)
  \add <node> <addr>   add a node (admin)
  \remove <node>       remove a node (admin)
  \schedule <ops>      test a schedule, e.g. \schedule r1(x) w2(x) w1(x)
  \stale               toggle stale reads from the connected node
  \q                   quit`)
	case `\stale`:
		s.stale = !s.stale
		fmt.Println("stale reads:", s.stale)
	case `\status`:
		r, err := s.cl.Admin(ctx, &protocol.AdminRequest{Op: "status"})
		if err != nil {
			fmt.Println("error:", err)
			return false
		}
		b, _ := json.MarshalIndent(r.Status, "", "  ")
		fmt.Println(string(b))
	case `\transfer`, `\add`, `\remove`:
		req := &protocol.AdminRequest{}
		switch {
		case f[0] == `\transfer` && len(f) == 2:
			req.Op, req.Node = "transfer", f[1]
		case f[0] == `\add` && len(f) == 3:
			req.Op, req.Node, req.Addr = "add_node", f[1], f[2]
		case f[0] == `\remove` && len(f) == 2:
			req.Op, req.Node = "remove_node", f[1]
		default:
			fmt.Println(`usage: \transfer <node> | \add <node> <addr> | \remove <node>`)
			return false
		}
		r, err := s.cl.Admin(ctx, req)
		if err != nil {
			fmt.Println("error:", err)
			return false
		}
		fmt.Println(r.Message)
	case `\schedule`:
		printSchedule(strings.TrimSpace(strings.TrimPrefix(line, `\schedule`)))
	default:
		fmt.Printf("unknown command %s; try \\help\n", f[0])
	}
	return false
}

func (s *shell) run(q string) bool {
	ctx := context.Background()
	up := strings.ToUpper(strings.TrimRight(strings.TrimSpace(q), "; "))
	var r *client.Result
	var err error
	switch {
	case up == "BEGIN":
		if s.tx != nil {
			fmt.Println("error: a transaction is already open")
			return false
		}
		s.tx, err = s.cl.Begin(ctx)
		if err == nil {
			fmt.Println("transaction started")
		}
	case up == "COMMIT" && s.tx != nil:
		err = s.tx.Commit()
		s.tx = nil
		if err == nil {
			fmt.Println("transaction committed")
		}
	case up == "ROLLBACK" && s.tx != nil:
		s.tx.Rollback()
		s.tx = nil
		fmt.Println("transaction rolled back")
	case s.tx != nil:
		r, err = s.tx.Exec(q)
		var ce *client.Error
		if errors.As(err, &ce) && (ce.Code == protocol.CodeTxnAborted || ce.Code == protocol.CodeUnknownOutcome) {
			s.tx = nil
		}
	case s.stale:
		r, err = s.cl.ExecStale(ctx, q)
	default:
		r, err = s.cl.Exec(ctx, q)
	}
	if err != nil {
		fmt.Println("error:", err)
		return false
	}
	if r != nil {
		printResult(r)
	}
	return true
}

func printResult(r *client.Result) {
	if r.Message != "" {
		fmt.Println(r.Message)
	}
	if len(r.Columns) > 0 {
		printTable(r.Columns, r.Rows)
		note := ""
		if r.Stale {
			note = fmt.Sprintf(", stale read from %s", r.Node)
		}
		fmt.Printf("(%d row%s%s)\n", len(r.Rows), plural(len(r.Rows)), note)
	} else if r.Message == "" {
		fmt.Printf("%d row%s affected\n", r.Affected, plural(r.Affected))
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func printTable(cols []string, rows [][]any) {
	width := make([]int, len(cols))
	for i, c := range cols {
		width[i] = len(c)
	}
	cells := make([][]string, len(rows))
	for i, row := range rows {
		cells[i] = make([]string, len(row))
		for j, v := range row {
			s := "NULL"
			if v != nil {
				s = fmt.Sprint(v)
			}
			cells[i][j] = s
			if len(s) > width[j] {
				width[j] = len(s)
			}
		}
	}
	line := func(vals []string) {
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = fmt.Sprintf("%-*s", width[i], v)
		}
		fmt.Println(" " + strings.Join(parts, " | "))
	}
	line(cols)
	seps := make([]string, len(cols))
	for i, w := range width {
		seps[i] = strings.Repeat("-", w)
	}
	fmt.Println("-" + strings.Join(seps, "-+-") + "-")
	for _, r := range cells {
		line(r)
	}
}
