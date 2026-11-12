// Package protocol defines the client wire format: one JSON object per line, over TLS.
package protocol

import (
	"encoding/json"
	"fmt"
	"strconv"

	"sentineldb/internal/sql"
)

// Role says what a client token may do. Higher roles include lower ones.
type Role int

const (
	RoleNone Role = iota
	RoleRead
	RoleWrite
	RoleAdmin
)

// ParseRole reads a role name from the tokens file.
func ParseRole(s string) (Role, error) {
	switch s {
	case "read":
		return RoleRead, nil
	case "write":
		return RoleWrite, nil
	case "admin":
		return RoleAdmin, nil
	}
	return RoleNone, fmt.Errorf("unknown role %q", s)
}

// Error codes tell a client whether trying again can help.
const (
	CodeAuth           = "auth"            // bad token or insufficient role
	CodeSyntax         = "syntax"          // the query is not in the grammar
	CodeError          = "error"           // the statement failed; retrying will not help
	CodeNotLeader      = "not_leader"      // safe to retry: nothing was proposed
	CodeNoLeader       = "no_leader"       // safe to retry: nothing was proposed
	CodeRetry          = "retry"           // safe to retry: nothing was proposed
	CodeTxnAborted     = "txn_aborted"     // the transaction is gone; rerun all of it
	CodeUnknownOutcome = "unknown_outcome" // may have committed; retry only with the same request ID
)

// Request is a client message. Exactly one of Auth, SQL or Admin is meaningful.
type Request struct {
	Auth   string `json:"auth,omitempty"`
	SQL    string `json:"sql,omitempty"`
	Params []any  `json:"params,omitempty"`
	// RequestID makes a write safe to retry: it is applied at most once.
	RequestID string `json:"request_id,omitempty"`
	// Stale asks for this node's local copy, possibly out of date, for speed.
	Stale bool          `json:"stale,omitempty"`
	Admin *AdminRequest `json:"admin,omitempty"`
	// ForwardRole is set by a node relaying for its client, over the mutually
	// authenticated peer port only. It is ignored on the client port.
	ForwardRole Role `json:"forward_role,omitempty"`
}

// AdminRequest is an authenticated administrative action.
type AdminRequest struct {
	Op   string `json:"op"` // status, add_node, remove_node, transfer, chaos
	Node string `json:"node,omitempty"`
	Addr string `json:"addr,omitempty"`
	// Chaos settings: the fault to inject on this node's outgoing peer traffic.
	Chaos *ChaosSpec `json:"chaos,omitempty"`
}

// ChaosSpec describes injected degradation. Nodes accept it only with --enable-chaos.
type ChaosSpec struct {
	DelayMs  int     `json:"delay_ms"`
	JitterMs int     `json:"jitter_ms"`
	DropRate float64 `json:"drop_rate"`
	// OnlyTo restricts the fault to traffic toward one peer, to model a single bad link.
	OnlyTo string `json:"only_to,omitempty"`
}

// Response is a server message.
type Response struct {
	OK        bool     `json:"ok"`
	Code      string   `json:"code,omitempty"`
	Error     string   `json:"error,omitempty"`
	Columns   []string `json:"columns,omitempty"`
	Rows      [][]any  `json:"rows,omitempty"`
	Affected  int      `json:"affected,omitempty"`
	Message   string   `json:"message,omitempty"`
	Stale     bool     `json:"stale,omitempty"`
	Duplicate bool     `json:"duplicate,omitempty"`
	InTxn     bool     `json:"in_txn,omitempty"`
	Node      string   `json:"node,omitempty"`
	Leader    string   `json:"leader,omitempty"`

	Status *NodeStatus `json:"status,omitempty"`
}

// NodeStatus is one node's view of the cluster, for operators and the dashboard.
type NodeStatus struct {
	ID        string            `json:"id"`
	Role      string            `json:"role"`
	Term      uint64            `json:"term"`
	Leader    string            `json:"leader"`
	Commit    uint64            `json:"commit"`
	Applied   uint64            `json:"applied"`
	LastIndex uint64            `json:"last_index"`
	Members   map[string]string `json:"members"`
	Tables    []string          `json:"tables,omitempty"`
	// Monitor summarizes the failure predictor on this node.
	Monitor *MonitorStatus `json:"monitor,omitempty"`
}

// MonitorStatus exposes the predictor's latest reading.
type MonitorStatus struct {
	Enabled     bool               `json:"enabled"`
	Model       string             `json:"model,omitempty"`
	Probability float64            `json:"probability"`
	Features    map[string]float64 `json:"features,omitempty"`
	Handovers   int                `json:"handovers"`
}

// EncodeValue converts a SQL value to its JSON form.
func EncodeValue(v sql.Value) any {
	switch v.K {
	case sql.KindInt:
		// A decimal string in json.Number form stays exact past 2^53, unlike a float.
		return json.Number(strconv.FormatInt(v.I, 10))
	case sql.KindText:
		return v.S
	case sql.KindBool:
		return v.B
	}
	return nil
}

// DecodeValue converts a JSON parameter to a SQL value.
func DecodeValue(a any) (sql.Value, error) {
	switch x := a.(type) {
	case nil:
		return sql.Null, nil
	case bool:
		return sql.Bool(x), nil
	case string:
		return sql.Text(x), nil
	case json.Number:
		i, err := strconv.ParseInt(x.String(), 10, 64)
		if err != nil {
			return sql.Null, fmt.Errorf("parameter %s is not a 64-bit integer", x)
		}
		return sql.Int(i), nil
	case int:
		return sql.Int(int64(x)), nil
	case int64:
		return sql.Int(x), nil
	}
	return sql.Null, fmt.Errorf("unsupported parameter type %T", a)
}

// DecodeParams converts all of a request's parameters.
func DecodeParams(params []any) ([]sql.Value, error) {
	out := make([]sql.Value, len(params))
	for i, p := range params {
		v, err := DecodeValue(p)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// EncodeRows converts result rows to their JSON form.
func EncodeRows(rows [][]sql.Value) [][]any {
	if rows == nil {
		return nil
	}
	out := make([][]any, len(rows))
	for i, r := range rows {
		cells := make([]any, len(r))
		for j, v := range r {
			cells[j] = EncodeValue(v)
		}
		out[i] = cells
	}
	return out
}
