package sqlparser

import (
	"fmt"
	"strings"
)

// tokenKind is the lexical class of a token. Keywords are not a separate
// class: the parser recognizes them contextually from identifier tokens, so
// keywords are case-insensitive while table and column names keep their case.
type tokenKind uint8

const (
	tokenEOF tokenKind = iota
	tokenIdent
	tokenInt
	tokenString
	tokenLParen
	tokenRParen
	tokenComma
	tokenSemicolon
	tokenStar
	tokenEq
	tokenPlus
	tokenMinus
	tokenDot
)

// token is one lexical token and its byte offset in the input.
type token struct {
	kind tokenKind
	text string // identifier spelling, digits, or decoded string contents
	pos  int    // byte offset of the first byte of the token
}

// lex converts sql into tokens terminated by a tokenEOF. It reports lexical
// errors (unterminated strings, unexpected characters) as *ParseError.
func lex(sql string) ([]token, *ParseError) {
	toks := make([]token, 0, len(sql)/3+1)
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case isSpace(c):
			i++

		case isIdentStart(c):
			start := i
			for i < len(sql) && isIdentPart(sql[i]) {
				i++
			}
			toks = append(toks, token{kind: tokenIdent, text: sql[start:i], pos: start})

		case c >= '0' && c <= '9':
			start := i
			for i < len(sql) && sql[i] >= '0' && sql[i] <= '9' {
				i++
			}
			toks = append(toks, token{kind: tokenInt, text: sql[start:i], pos: start})

		case c == '\'':
			start := i
			i++ // opening quote
			var sb strings.Builder
			closed := false
			for i < len(sql) {
				if sql[i] == '\'' {
					// A doubled quote is an escaped single quote.
					if i+1 < len(sql) && sql[i+1] == '\'' {
						sb.WriteByte('\'')
						i += 2
						continue
					}
					i++ // closing quote
					closed = true
					break
				}
				sb.WriteByte(sql[i])
				i++
			}
			if !closed {
				return nil, &ParseError{Pos: start, Msg: "unterminated string literal"}
			}
			toks = append(toks, token{kind: tokenString, text: sb.String(), pos: start})

		default:
			kind := tokenEOF
			switch c {
			case '(':
				kind = tokenLParen
			case ')':
				kind = tokenRParen
			case ',':
				kind = tokenComma
			case ';':
				kind = tokenSemicolon
			case '*':
				kind = tokenStar
			case '=':
				kind = tokenEq
			case '+':
				kind = tokenPlus
			case '-':
				kind = tokenMinus
			case '.':
				kind = tokenDot
			default:
				return nil, &ParseError{Pos: i, Msg: fmt.Sprintf("unexpected character %q", c)}
			}
			toks = append(toks, token{kind: kind, text: string(c), pos: i})
			i++
		}
	}
	toks = append(toks, token{kind: tokenEOF, pos: len(sql)})
	return toks, nil
}

func isSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}
