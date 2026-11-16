package internal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strconv"
	"strings"
	"unicode"

	"github.com/goccy/googlesqlite/internal/value"
)

// formatScriptInt / formatScriptFloat keep the literal forms small
// and analyzer-friendly when DECLARE/SET RHS evaluates to a
// numeric.
func formatScriptInt(v int64) string {
	return strconv.FormatInt(v, 10)
}

func formatScriptFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// applyScriptVariables intercepts BigQuery script DECLARE / SET
// statements before they reach the upstream analyzer, which would
// otherwise reject them with "Statement not supported:
// VariableDeclaration / SingleAssignment". Resolved-AST level
// support for ResolvedScript is not exposed by go-googlesql yet.
//
// Behaviour:
//
//   - `DECLARE name [type] [DEFAULT expr] ;`
//     records (name, expr_text) on Conn.scriptVars (lowercased name)
//     and removes the statement from the rewritten SQL.
//
//   - `SET name = expr ;` (NOT @@name — those are system variables)
//     overwrites the recorded expr; removes the statement.
//
//   - Every other statement keeps its place in the rewritten SQL,
//     with bare references to known variable names replaced by the
//     current expr (case-insensitive identifier lookup, skipping
//     string / backtick / single-quote literals).
//
// Limitations:
//
//   - DECLARE list form (`DECLARE a, b INT64`) is parsed but only
//     the first identifier participates in the substitution map;
//     the others are silently ignored. Call out in test coverage if
//     the multi-ident form needs full support.
//   - Variable references are substituted positionally as text,
//     so an ARRAY-typed variable can be inlined as `[1, 2, 3]` and
//     pass through ARRAY-shaped contexts but a STRUCT variable
//     used in a complicated expression may need parens.
//
// Substitutions appear in the rewritten SQL parenthesised so binary
// expressions like `id = x` survive when x is e.g. `1 + 2`.
//
// Query parameters on a DECLARE DEFAULT / SET right-hand side are bound
// from args when the expression is evaluated: named ones by name, and
// positional ones in the order their `?` appears in the script. The
// positional values a DECLARE / SET consumes are removed from the
// returned args, because its statement leaves the rewritten SQL and the
// remaining statements must still receive their own values in order.
func applyScriptVariables(ctx context.Context, query string, conn *Conn, args []driver.NamedValue) (string, []driver.NamedValue) {
	if conn == nil {
		return query, args
	}
	stmts := splitTopLevelStatements(query)
	if len(stmts) == 0 {
		return query, args
	}
	// positional holds the indexes into args of the positional values
	// in order; next is the first one no statement has claimed yet.
	var positional []int
	for i, v := range args {
		if v.Name == "" {
			positional = append(positional, i)
		}
	}
	next := 0
	consumed := map[int]struct{}{}
	// evaluate substitutes earlier variables into expr and evaluates
	// it with the parameters it references bound. When a referenced
	// parameter is not available (e.g. at Prepare time, before args
	// are bound) it keeps the parenthesised text instead, so the
	// placeholder lands in the statement that uses the variable and is
	// bound or rejected there; the positional values stay unclaimed.
	evaluate := func(expr string) string {
		expr = substituteScriptVariables(expr, conn)
		bound, nPositional, ok := bindScriptParams(expr, args, positional[next:])
		if ok {
			if lit, ok := evaluateScriptVariableExpr(ctx, conn, expr, bound); ok {
				for _, idx := range positional[next : next+nPositional] {
					consumed[idx] = struct{}{}
				}
				next += nPositional
				return lit
			}
		}
		return parenthesizeScriptExpr(expr)
	}
	var keep []string
	for _, stmt := range stmts {
		trimmed := strings.TrimSpace(stmt)
		if trimmed == "" {
			continue
		}
		if name, expr, ok := parseDeclare(trimmed); ok {
			// Substitute any earlier-declared variables on the RHS,
			// evaluate the result through SQLite to a concrete
			// literal, and store the literal. This keeps repeated
			// SET m = m + 1 from growing the text quadratically.
			conn.SetScriptVariable(name, evaluate(expr))
			continue
		}
		if name, expr, ok := parseAssignment(trimmed); ok {
			if _, declared := conn.ScriptVariable(name); declared {
				conn.SetScriptVariable(name, evaluate(expr))
				continue
			}
			// Not a declared variable — fall through (handled by
			// AssignmentStmtAction for @@vars; user error otherwise).
		}
		// Substitute any known variable into the surviving statement,
		// and let it claim the positional values for its own `?`s.
		kept := substituteScriptVariables(stmt, conn)
		_, n := scanScriptParamRefs(kept)
		next = min(next+n, len(positional))
		keep = append(keep, kept)
	}
	if len(consumed) > 0 {
		// Drop the values the DECLARE / SET statements bound and
		// renumber the rest so the ordinals stay contiguous.
		out := make([]driver.NamedValue, 0, len(args)-len(consumed))
		for i, v := range args {
			if _, ok := consumed[i]; ok {
				continue
			}
			v.Ordinal = len(out) + 1
			out = append(out, v)
		}
		args = out
	}
	if len(keep) == 0 {
		// Pure DECLARE / SET script: feed the analyzer a no-op
		// SELECT so it has something to do.
		return "SELECT 1", args
	}
	return strings.Join(keep, "; "), args
}

// bindScriptParams returns the arguments to evaluate a script variable
// expression with: a sql.NamedArg for each `@name` it references, and
// the next positional values in order for its `?`s. ok is false when a
// referenced parameter is missing, or is written in a form this scan
// does not recognise, so the caller must not evaluate the expression
// (SQLite would silently read an unbound named parameter as NULL).
func bindScriptParams(expr string, args []driver.NamedValue, positional []int) (bound []any, nPositional int, ok bool) {
	names, nPositional := scanScriptParamRefs(expr)
	if nPositional > len(positional) {
		return nil, 0, false
	}
	for _, idx := range positional[:nPositional] {
		bound = append(bound, args[idx].Value)
	}
	seen := map[string]struct{}{}
	for _, name := range names {
		if name == "" {
			return nil, 0, false
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		found := false
		for _, v := range args {
			if v.Name != "" && strings.EqualFold(v.Name, name) {
				bound = append(bound, sql.Named(name, v.Value))
				found = true
				break
			}
		}
		if !found {
			return nil, 0, false
		}
	}
	return bound, nPositional, true
}

// scanScriptParamRefs returns the `@name` parameter references in expr
// as written, and the number of positional `?` placeholders, skipping
// quoted literals and `@@` system variables. A reference it cannot
// name, such as a backticked `@`name“, is reported as "".
func scanScriptParamRefs(expr string) (names []string, nPositional int) {
	for i := 0; i < len(expr); {
		c := expr[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			i = scanQuoted(expr, i, c)
		case c == '?':
			nPositional++
			i++
		case c == '@' && i+1 < len(expr) && expr[i+1] == '@':
			// System variable: skip the `@@` and its name.
			i += 2
			i += scanIdent(expr[i:])
		case c == '@':
			n := scanIdent(expr[i+1:])
			if n == 0 && i+1 < len(expr) && expr[i+1] == '`' {
				names = append(names, "")
			} else if n > 0 {
				names = append(names, expr[i+1:i+1+n])
			}
			i += 1 + n
		default:
			i++
		}
	}
	return names, nPositional
}

// parenthesizeScriptExpr is the fallback form of a script variable
// whose expression could not be evaluated on its own.
func parenthesizeScriptExpr(expr string) string {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return "NULL"
	}
	return "(" + expr + ")"
}

// evaluateScriptVariableExpr asks the underlying SQLite to compute
// a single-row, single-column result for the given expression, with
// args bound to its parameters, and returns the result as a SQL
// literal. ok is false when evaluation fails; callers then fall back to
// a parenthesised version of the original text, which keeps them
// correct even for expressions whose only use is in another statement
// (where SQLite's evaluator might reject them in isolation).
func evaluateScriptVariableExpr(ctx context.Context, conn *Conn, expr string, args []any) (string, bool) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return "NULL", true
	}
	row := conn.QueryRowContext(ctx, "SELECT "+expr, args...)
	var raw any
	if err := row.Scan(&raw); err != nil {
		return "", false
	}
	switch v := raw.(type) {
	case nil:
		return "NULL", true
	case int64:
		return formatScriptInt(v), true
	case float64:
		return formatScriptFloat(v), true
	case bool:
		if v {
			return "TRUE", true
		}
		return "FALSE", true
	case string:
		// A bound query parameter arrives in the encoded envelope
		// layout. A STRING one becomes a GoogleSQL string literal of
		// its decoded text, escaped because the text is the value
		// itself rather than literal source. Other encoded types
		// have no literal form here, so the caller inlines the
		// expression and its placeholder instead.
		if decoded, err := DecodeValue(v); err == nil {
			s, isString := decoded.(value.StringValue)
			if !isString {
				return "", false
			}
			if string(s) != v {
				return strconv.Quote(string(s)), true
			}
		}
		// A plain string from a literal in the expression. Quote with
		// double quotes (DQS_DML is enabled), matching the form
		// LiteralFromValue emits for non-primitive values.
		return "\"" + strings.ReplaceAll(v, "\"", "\\\"") + "\"", true
	case []byte:
		return "\"" + strings.ReplaceAll(string(v), "\"", "\\\"") + "\"", true
	}
	return "", false
}

// splitTopLevelStatements splits on `;` while respecting single
// quotes, double quotes and backticks. Triple-quoted strings are
// not common in scripts; the simple form here is enough for the
// statement boundaries the rewriter needs.
func splitTopLevelStatements(query string) []string {
	var stmts []string
	start := 0
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch c {
		case '\'', '"', '`':
			end := scanQuoted(query, i, c)
			i = end - 1
		case ';':
			stmts = append(stmts, query[start:i])
			start = i + 1
		}
	}
	if start < len(query) {
		stmts = append(stmts, query[start:])
	}
	return stmts
}

// scanQuoted returns the byte index past the closing quote at
// query[start]. Bare backslash escapes are honoured for ' and ";
// backticks have no escape mechanism but tolerate doubled “ to
// embed a backtick.
func scanQuoted(query string, start int, quote byte) int {
	i := start + 1
	for i < len(query) {
		c := query[i]
		if c == '\\' && quote != '`' && i+1 < len(query) {
			i += 2
			continue
		}
		if c == quote {
			return i + 1
		}
		i++
	}
	return len(query)
}

// parseDeclare matches `DECLARE name [type] [DEFAULT expr]` and
// returns the (lower-cased) identifier and the DEFAULT expr text.
// Returns ok=false when the statement is not a DECLARE we can
// handle.
func parseDeclare(stmt string) (name, expr string, ok bool) {
	rest, hasKW := matchKeyword(stmt, "DECLARE")
	if !hasKW {
		return "", "", false
	}
	rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
	identEnd := scanIdent(rest)
	if identEnd == 0 {
		return "", "", false
	}
	name = strings.ToLower(rest[:identEnd])
	rest = rest[identEnd:]
	rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
	// `DECLARE x DEFAULT 5` (no type)? `DECLARE x INT64 DEFAULT 5`?
	// Walk forward: skip until DEFAULT or end of statement.
	defaultIdx := indexOfKeyword(rest, "DEFAULT")
	if defaultIdx < 0 {
		// No DEFAULT — start the variable as NULL.
		return name, "NULL", true
	}
	expr = strings.TrimSpace(rest[defaultIdx+len("DEFAULT"):])
	expr = strings.TrimSuffix(expr, ";")
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return name, "NULL", true
	}
	return name, expr, true
}

// parseAssignment matches `SET name = expr` for non-@@ targets.
func parseAssignment(stmt string) (name, expr string, ok bool) {
	rest, hasKW := matchKeyword(stmt, "SET")
	if !hasKW {
		return "", "", false
	}
	rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
	if strings.HasPrefix(rest, "@@") {
		// System-variable assignment is handled elsewhere.
		return "", "", false
	}
	identEnd := scanIdent(rest)
	if identEnd == 0 {
		return "", "", false
	}
	name = strings.ToLower(rest[:identEnd])
	rest = strings.TrimLeftFunc(rest[identEnd:], unicode.IsSpace)
	if !strings.HasPrefix(rest, "=") {
		return "", "", false
	}
	expr = strings.TrimSpace(rest[1:])
	expr = strings.TrimSuffix(expr, ";")
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return "", "", false
	}
	return name, expr, true
}

// substituteScriptVariables walks `stmt` and replaces every bare
// identifier that matches a declared script variable name (case-
// insensitive) with the variable's stored expression. Quoted
// strings, backticked identifiers, and tokens prefixed with `.`
// (struct field access) or `@` (parameter) are left alone.
func substituteScriptVariables(stmt string, conn *Conn) string {
	if conn == nil || len(conn.scriptVars) == 0 {
		return stmt
	}
	var b strings.Builder
	b.Grow(len(stmt))
	i := 0
	for i < len(stmt) {
		c := stmt[i]
		switch c {
		case '\'', '"', '`':
			end := scanQuoted(stmt, i, c)
			b.WriteString(stmt[i:end])
			i = end
			continue
		}
		// Skip identifiers that are field accessors (`a.x`) — only
		// substitute when the name stands alone.
		if c == '.' {
			b.WriteByte(c)
			i++
			// Skip the following identifier verbatim.
			j := i
			for j < len(stmt) && (isIdentCont(stmt[j])) {
				j++
			}
			b.WriteString(stmt[i:j])
			i = j
			continue
		}
		if isIdentStart(c) && (i == 0 || !isIdentCont(stmt[i-1])) {
			j := i + 1
			for j < len(stmt) && isIdentCont(stmt[j]) {
				j++
			}
			word := stmt[i:j]
			if expr, ok := conn.ScriptVariable(strings.ToLower(word)); ok {
				// Don't substitute when followed by `(` — that would
				// rewrite a function call whose name happens to
				// collide with a declared variable.
				k := j
				for k < len(stmt) && (stmt[k] == ' ' || stmt[k] == '\t') {
					k++
				}
				if k < len(stmt) && stmt[k] == '(' {
					b.WriteString(word)
				} else {
					b.WriteString(expr)
				}
			} else {
				b.WriteString(word)
			}
			i = j
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// matchKeyword returns the suffix after a leading <kw> keyword if
// the statement starts with it (case-insensitive, word-boundary
// aware). Returns the remainder and true on match.
func matchKeyword(stmt, kw string) (string, bool) {
	stmt = strings.TrimLeftFunc(stmt, unicode.IsSpace)
	if len(stmt) < len(kw) {
		return "", false
	}
	if !strings.EqualFold(stmt[:len(kw)], kw) {
		return "", false
	}
	if len(stmt) > len(kw) && isIdentCont(stmt[len(kw)]) {
		return "", false
	}
	return stmt[len(kw):], true
}

// indexOfKeyword finds the byte offset of a top-level <kw> keyword
// in s (case-insensitive, word-boundary aware), respecting string
// literals. Returns -1 when absent.
func indexOfKeyword(s, kw string) int {
	for i := 0; i < len(s); {
		c := s[i]
		switch c {
		case '\'', '"', '`':
			i = scanQuoted(s, i, c)
			continue
		}
		if isIdentStart(c) {
			j := i + 1
			for j < len(s) && isIdentCont(s[j]) {
				j++
			}
			if strings.EqualFold(s[i:j], kw) {
				return i
			}
			i = j
			continue
		}
		i++
	}
	return -1
}

// scanIdent returns the byte length of the leading identifier in s,
// or 0 when s starts with something that is not an identifier.
func scanIdent(s string) int {
	if len(s) == 0 || !isIdentStart(s[0]) {
		return 0
	}
	i := 1
	for i < len(s) && isIdentCont(s[i]) {
		i++
	}
	return i
}
