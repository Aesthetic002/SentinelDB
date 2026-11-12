package sql

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// TokenKind classifies a lexical token.
type TokenKind uint8

const (
	TokEOF TokenKind = iota
	TokKeyword
	TokIdent
	TokInt
	TokString
	TokParam  // ?
	TokSymbol // punctuation and operators
)

// Token is one lexical unit. Pos is its byte offset in the query, for error messages.
type Token struct {
	Kind TokenKind
	Text string // keywords are upper case, identifiers lower case, strings unescaped
	Int  int64
	Pos  int
}

func (t Token) String() string {
	switch t.Kind {
	case TokEOF:
		return "end of input"
	case TokString:
		return "string '" + t.Text + "'"
	case TokKeyword:
		return "keyword " + t.Text
	case TokIdent:
		return "identifier " + t.Text
	}
	return "'" + t.Text + "'"
}

var keywords = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "JOIN": true, "ON": true, "AS": true,
	"INNER": true, "ORDER": true, "BY": true, "ASC": true, "DESC": true, "LIMIT": true,
	"INSERT": true, "INTO": true, "VALUES": true,
	"UPDATE": true, "SET": true, "DELETE": true,
	"CREATE": true, "TABLE": true, "PRIMARY": true, "KEY": true, "FOREIGN": true,
	"REFERENCES": true, "NOT": true, "NULL": true,
	"INT": true, "INTEGER": true, "TEXT": true, "BOOL": true, "BOOLEAN": true,
	"AND": true, "OR": true, "IS": true, "TRUE": true, "FALSE": true,
	"BEGIN": true, "COMMIT": true, "ROLLBACK": true, "EXPLAIN": true,
}

// MaxQueryBytes bounds a query's size so a hostile client cannot exhaust memory in
// the lexer.
const MaxQueryBytes = 1 << 20

// Lex converts query text to tokens. It always ends the result with a TokEOF token.
func Lex(src string) ([]Token, error) {
	if len(src) > MaxQueryBytes {
		return nil, fmt.Errorf("query is %d bytes, over the %d byte limit", len(src), MaxQueryBytes)
	}
	if !utf8.ValidString(src) {
		return nil, fmt.Errorf("query is not valid UTF-8")
	}
	var toks []Token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case isIdentStart(c):
			start := i
			for i < len(src) && isIdentPart(src[i]) {
				i++
			}
			word := src[start:i]
			if up := strings.ToUpper(word); keywords[up] {
				toks = append(toks, Token{Kind: TokKeyword, Text: up, Pos: start})
			} else {
				toks = append(toks, Token{Kind: TokIdent, Text: strings.ToLower(word), Pos: start})
			}
		case c >= '0' && c <= '9':
			start := i
			for i < len(src) && src[i] >= '0' && src[i] <= '9' {
				i++
			}
			if i < len(src) && isIdentStart(src[i]) {
				return nil, fmt.Errorf("at byte %d: malformed number", start)
			}
			n, err := strconv.ParseInt(src[start:i], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("at byte %d: integer %s out of range", start, src[start:i])
			}
			toks = append(toks, Token{Kind: TokInt, Text: src[start:i], Int: n, Pos: start})
		case c == '\'':
			start := i
			i++
			var b strings.Builder
			closed := false
			for i < len(src) {
				if src[i] != '\'' {
					b.WriteByte(src[i])
					i++
					continue
				}
				if i+1 < len(src) && src[i+1] == '\'' { // '' is an escaped quote
					b.WriteByte('\'')
					i += 2
					continue
				}
				i++
				closed = true
				break
			}
			if !closed {
				return nil, fmt.Errorf("at byte %d: unterminated string", start)
			}
			toks = append(toks, Token{Kind: TokString, Text: b.String(), Pos: start})
		case c == '?':
			toks = append(toks, Token{Kind: TokParam, Text: "?", Pos: i})
			i++
		default:
			two := ""
			if i+1 < len(src) {
				two = src[i : i+2]
			}
			switch two {
			case "<=", ">=", "<>", "!=":
				if two == "!=" {
					two = "<>"
				}
				toks = append(toks, Token{Kind: TokSymbol, Text: two, Pos: i})
				i += 2
				continue
			}
			if strings.IndexByte("()*,;.=<>+-/", c) < 0 {
				r, _ := utf8.DecodeRuneInString(src[i:])
				return nil, fmt.Errorf("at byte %d: unexpected character %q", i, r)
			}
			toks = append(toks, Token{Kind: TokSymbol, Text: string(c), Pos: i})
			i++
		}
	}
	return append(toks, Token{Kind: TokEOF, Pos: len(src)}), nil
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isIdentPart(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }
