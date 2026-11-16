package internal

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strconv"
	"strings"
	"unicode"
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
// A right-hand side that references a query parameter is not evaluated:
// its text is stored instead (cast to the declared type, if any), so the
// statement that uses the variable binds the parameter with its real
// type. applyScriptPositionalRewrite has already renamed any positional
// `?` there to a named parameter.
func applyScriptVariables(ctx context.Context, query string, conn *Conn) string {
	if conn == nil {
		return query
	}
	stmts := splitTopLevelStatements(query)
	if len(stmts) == 0 {
		return query
	}
	var keep []string
	for _, stmt := range stmts {
		trimmed := trimLeadingComments(stmt)
		if trimmed == "" {
			continue
		}
		if name, typ, expr, ok := parseDeclare(trimmed); ok {
			// Substitute any earlier-declared variables on the RHS
			// and store its value; see scriptVariableValue.
			conn.SetScriptVariable(name, scriptVariableValue(ctx, conn, typ, expr))
			continue
		}
		if name, expr, ok := parseAssignment(trimmed); ok {
			if _, declared := conn.ScriptVariable(name); declared {
				conn.SetScriptVariable(name, scriptVariableValue(ctx, conn, "", expr))
				continue
			}
			// Not a declared variable — fall through (handled by
			// AssignmentStmtAction for @@vars; user error otherwise).
		}
		// Substitute any known variable into the surviving statement.
		keep = append(keep, substituteScriptVariables(stmt, conn))
	}
	if len(keep) == 0 {
		// Pure DECLARE / SET script: feed the analyzer a no-op
		// SELECT so it has something to do.
		return "SELECT 1"
	}
	return strings.Join(keep, "; ")
}

// scriptVariableValue returns the text a script variable assigned expr
// is substituted with. A parameter-free expression is evaluated through
// SQLite to a literal, which keeps repeated SET m = m + 1 from growing
// the text quadratically. One that references a query parameter is kept
// as text, cast to the declared type typ when there is one: SQLite would
// read the parameter unbound (as NULL), and inlining the text lets the
// statement that uses the variable bind the parameter by name, with its
// real type, as often as the variable appears.
//
// Known limitation: the inlined text is evaluated at each use rather
// than once, so a subquery or volatile function (e.g. RAND()) in a
// parameterized right-hand side can give a different value than in
// BigQuery, which evaluates a DECLARE / SET once.
func scriptVariableValue(ctx context.Context, conn *Conn, typ, expr string) string {
	expr = substituteScriptVariables(expr, conn)
	if !hasQueryParameter(expr) {
		return evaluateScriptVariableExpr(ctx, conn, expr)
	}
	// Comments are dropped because the text is wrapped and inlined: a
	// trailing `-- note` would otherwise comment out the closing
	// parenthesis and the rest of the using statement.
	expr = strings.TrimSpace(stripComments(expr, true))
	if typ = strings.TrimSpace(stripComments(typ, true)); typ != "" {
		// CAST is more lenient than BigQuery's assignment coercion
		// (e.g. it accepts a STRING for an INT64 variable), and a
		// parameterized type such as NUMERIC(10, 2) or STRING(5) is not
		// enforced.
		return "CAST((" + expr + ") AS " + typ + ")"
	}
	return "(" + expr + ")"
}

// hasQueryParameter reports whether expr references a named or
// positional query parameter outside literals and comments. `@@name`
// system variables do not count.
func hasQueryParameter(expr string) bool {
	code := strings.ReplaceAll(stripLiteralsAndComments(expr), "@@", "")
	return strings.ContainsAny(code, "?@")
}

// applyScriptPositionalRewrite renames every positional `?` in a script
// to a named parameter, and names the positional args to match, when a
// DECLARE DEFAULT / SET right-hand side uses one. applyScriptVariables
// inlines such a right-hand side wherever the variable is used, so its
// placeholder must bind by name rather than by position. Positional
// parameters bind in the order they appear in the script, which the
// renaming preserves.
//
// A prepared statement has no args yet, and binds its values in the
// order its own placeholders appear, which would no longer match, so
// that case (like any call with too few values) is rejected.
func applyScriptPositionalRewrite(query string, args []driver.NamedValue) (string, []driver.NamedValue, error) {
	usesPositional := false
	for _, stmt := range splitTopLevelStatements(query) {
		stmt = trimLeadingComments(stmt)
		_, _, expr, ok := parseDeclare(stmt)
		if !ok {
			_, expr, ok = parseAssignment(stmt)
		}
		if ok && strings.Contains(stripLiteralsAndComments(expr), "?") {
			usesPositional = true
			break
		}
	}
	if !usesPositional {
		return query, args, nil
	}
	nPositional := 0
	for _, v := range args {
		if v.Name == "" {
			nPositional++
		}
	}
	if want := strings.Count(stripLiteralsAndComments(query), "?"); nPositional < want {
		if len(args) == 0 {
			return "", nil, fmt.Errorf("not enough query arguments: positional parameters in DECLARE or SET are not supported in prepared statements")
		}
		return "", nil, fmt.Errorf("not enough query arguments")
	}
	query, args = renamePositionalParams(query, args)
	return query, args, nil
}

// stripLiteralsAndComments returns s with its string literals, quoted
// identifiers and comments removed, so placeholders can be counted.
func stripLiteralsAndComments(s string) string {
	return stripComments(s, false)
}

// stripComments returns s with its comments replaced by a space, and
// its string literals and quoted identifiers too unless keepLiterals.
func stripComments(s string, keepLiterals bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if end := skipLiteralOrComment(s, i); end > i {
			if keepLiterals && s[i] != '-' && s[i] != '#' && s[i] != '/' {
				b.WriteString(s[i:end])
			} else {
				b.WriteByte(' ')
			}
			i = end - 1
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// trimLeadingComments strips leading whitespace and comments from stmt.
func trimLeadingComments(stmt string) string {
	for {
		stmt = strings.TrimLeftFunc(stmt, unicode.IsSpace)
		if !strings.HasPrefix(stmt, "--") && !strings.HasPrefix(stmt, "#") && !strings.HasPrefix(stmt, "/*") {
			return strings.TrimSpace(stmt)
		}
		stmt = stmt[skipLiteralOrComment(stmt, 0):]
	}
}

// evaluateScriptVariableExpr asks the underlying SQLite to compute
// a single-row, single-column result for the given expression and
// returns the result as a SQL literal. Falls back to a parenthesised
// version of the original text when evaluation fails — that keeps
// callers correct even for expressions whose only use is in another
// statement (where SQLite's evaluator might reject them in isolation).
func evaluateScriptVariableExpr(ctx context.Context, conn *Conn, expr string) string {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return "NULL"
	}
	row := conn.QueryRowContext(ctx, "SELECT "+expr)
	var raw any
	if err := row.Scan(&raw); err != nil {
		return "(" + expr + ")"
	}
	switch v := raw.(type) {
	case nil:
		return "NULL"
	case int64:
		return formatScriptInt(v)
	case float64:
		return formatScriptFloat(v)
	case bool:
		if v {
			return "TRUE"
		}
		return "FALSE"
	case string:
		// May be an envelope-encoded value; if so the caller-site
		// substitution will store the envelope text and SQLite
		// will read it back via the standard collation. Quote with
		// double quotes (DQS_DML is enabled), matching the form
		// LiteralFromValue emits for non-primitive values.
		return "\"" + strings.ReplaceAll(v, "\"", "\\\"") + "\""
	case []byte:
		return "\"" + strings.ReplaceAll(string(v), "\"", "\\\"") + "\""
	}
	return "(" + expr + ")"
}

// splitTopLevelStatements splits on `;` while respecting single
// quotes, double quotes and backticks. Triple-quoted strings are
// not common in scripts; the simple form here is enough for the
// statement boundaries the rewriter needs.
func splitTopLevelStatements(query string) []string {
	var stmts []string
	start := 0
	for i := 0; i < len(query); i++ {
		if end := skipLiteralOrComment(query, i); end > i {
			i = end - 1
			continue
		}
		if query[i] == ';' {
			stmts = append(stmts, query[start:i])
			start = i + 1
		}
	}
	if start < len(query) {
		stmts = append(stmts, query[start:])
	}
	return stmts
}

// skipLiteralOrComment returns the byte index just past the string
// literal, quoted identifier or comment that starts at s[i], or i when
// none starts there. It handles single-, double- and triple-quoted
// strings, backticked identifiers, and `--`, `#` and `/* */` comments,
// so a `;`, `?` or apostrophe inside any of them is not mistaken for
// code. A backslash always skips the next character: even a raw string
// (r'a\'b') does not end at an escaped quote.
func skipLiteralOrComment(s string, i int) int {
	switch c := s[i]; {
	case c == '-' && strings.HasPrefix(s[i:], "--"), c == '#':
		if end := strings.IndexByte(s[i:], '\n'); end >= 0 {
			return i + end
		}
		return len(s)
	case c == '/' && strings.HasPrefix(s[i:], "/*"):
		if end := strings.Index(s[i+2:], "*/"); end >= 0 {
			return i + 2 + end + 2
		}
		return len(s)
	case c == '\'' || c == '"' || c == '`':
		quote := s[i : i+1]
		if c != '`' && strings.HasPrefix(s[i:], strings.Repeat(quote, 3)) {
			quote = strings.Repeat(quote, 3)
		}
		for j := i + len(quote); j < len(s); j++ {
			if s[j] == '\\' {
				j++
				continue
			}
			if strings.HasPrefix(s[j:], quote) {
				return j + len(quote)
			}
		}
		return len(s)
	}
	return i
}

// parseDeclare matches `DECLARE name [type] [DEFAULT expr]` and
// returns the (lower-cased) identifier, the type text ("" when absent
// or when the list form makes it ambiguous) and the DEFAULT expr text.
// Returns ok=false when the statement is not a DECLARE we can
// handle.
func parseDeclare(stmt string) (name, typ, expr string, ok bool) {
	rest, hasKW := matchKeyword(stmt, "DECLARE")
	if !hasKW {
		return "", "", "", false
	}
	rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
	identEnd := scanIdent(rest)
	if identEnd == 0 {
		return "", "", "", false
	}
	name = strings.ToLower(rest[:identEnd])
	rest = rest[identEnd:]
	rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
	// `DECLARE x DEFAULT 5` (no type)? `DECLARE x INT64 DEFAULT 5`?
	// Walk forward: skip until DEFAULT or end of statement.
	defaultIdx := indexOfKeyword(rest, "DEFAULT")
	if defaultIdx < 0 {
		// No DEFAULT — start the variable as NULL.
		return name, "", "NULL", true
	}
	if typ = strings.TrimSpace(rest[:defaultIdx]); strings.HasPrefix(typ, ",") {
		typ = ""
	}
	expr = strings.TrimSpace(rest[defaultIdx+len("DEFAULT"):])
	expr = strings.TrimSuffix(expr, ";")
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return name, typ, "NULL", true
	}
	return name, typ, expr, true
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
		if end := skipLiteralOrComment(stmt, i); end > i {
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
			// A name right after `@` or `@@` is a query parameter or a
			// system variable, never a script variable.
			if i > 0 && stmt[i-1] == '@' {
				b.WriteString(word)
				i = j
				continue
			}
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
		if end := skipLiteralOrComment(s, i); end > i {
			i = end
			continue
		}
		c := s[i]
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
