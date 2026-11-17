package internal

import (
	"context"
	"database/sql/driver"
	"fmt"
	"math"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"

	googlesql "github.com/goccy/go-googlesql"

	"github.com/goccy/googlesqlite/internal/value"
)

// Script variables (`DECLARE` / `SET name = expr`).
//
// The upstream analyzer refuses VariableDeclaration and SingleAssignment
// ("Statement not supported"): resolved-AST support for scripts is not
// exposed by go-googlesql. The driver therefore runs them itself:
//
//   - DECLARE and SET are statements of the script like any other. They
//     run in order, when the script reaches them, so an expression such
//     as `(SELECT MAX(id) FROM t)` sees what the statements before it
//     did. The expression is analyzed and executed through the regular
//     query path, and its value is stored on Conn.scriptVars as a typed
//     GoogleSQL literal.
//
//   - A statement that references a variable is resolved by the
//     analyzer first. Only a name the analyzer reports as unrecognized
//     is replaced by the variable's literal, at the exact location the
//     analyzer points to, and the statement is analyzed again. Column
//     names, aliases, table names, field accesses and function names
//     are never touched, and a column of the same name as a variable
//     takes precedence over it.
//
// Variables are kept on the driver connection, so a variable declared
// by one query can be read by the next query on the same connection.

// maxScriptVariableSubstitutions caps the analyze/substitute loop of a
// single statement.
const maxScriptVariableSubstitutions = 1000

var unrecognizedNameRe = regexp.MustCompile(`Unrecognized name: (` + "`" + `(?:[^` + "`" + `\\]|\\.)+` + "`" + `|[^\s;\[]+).*?\[at (\d+):(\d+)\]`)

// parseUnrecognizedName extracts the name and the 1-based line/column
// from an analyzer "Unrecognized name" error.
func parseUnrecognizedName(err error) (name string, line, col int, ok bool) {
	if err == nil {
		return "", 0, 0, false
	}
	m := unrecognizedNameRe.FindStringSubmatch(err.Error())
	if m == nil {
		return "", 0, 0, false
	}
	name = m[1]
	if len(name) >= 2 && name[0] == '`' && name[len(name)-1] == '`' {
		name = name[1 : len(name)-1]
	}
	line, lerr := strconv.Atoi(m[2])
	col, cerr := strconv.Atoi(m[3])
	if lerr != nil || cerr != nil {
		return "", 0, 0, false
	}
	return name, line, col, true
}

// nodeByteRange returns the byte range of node in its input.
func nodeByteRange(node googlesql.ASTNode) (int, int, bool) {
	if node == nil {
		return 0, 0, false
	}
	start, err := node.StartLocation()
	if err != nil || start == nil {
		return 0, 0, false
	}
	end, err := node.EndLocation()
	if err != nil || end == nil {
		return 0, 0, false
	}
	from, err := start.GetByteOffset()
	if err != nil {
		return 0, 0, false
	}
	to, err := end.GetByteOffset()
	if err != nil || from < 0 || to < from {
		return 0, 0, false
	}
	return int(from), int(to), true
}

// nodeText returns the text of node in query.
func nodeText(node googlesql.ASTNode, query string) (string, bool) {
	from, to, ok := nodeByteRange(node)
	if !ok || to > len(query) {
		return "", false
	}
	return query[from:to], true
}

// scriptVariableRef is a reference to a name that may be a script
// variable: the first identifier of a path expression (`v`, or `v` in
// the field access `v.field`).
type scriptVariableRef struct {
	name     string
	from, to int
}

// pathHead returns the first identifier of a path expression node.
func pathHead(n googlesql.ASTNode) (*googlesql.ASTIdentifier, bool) {
	path, ok := n.(*googlesql.ASTPathExpression)
	if !ok {
		return nil, false
	}
	if num, err := path.NumChildren(); err != nil || num < 1 {
		return nil, false
	}
	child, err := path.Child(0)
	if err != nil || child == nil {
		return nil, false
	}
	ident, ok := child.(*googlesql.ASTIdentifier)
	return ident, ok
}

// collectScriptVariableRefs returns the path expression heads of stmt
// named name (case-insensitive).
func collectScriptVariableRefs(stmt googlesql.ASTNode, name string) []scriptVariableRef {
	var refs []scriptVariableRef
	_ = astWalk(stmt, func(n googlesql.ASTNode) error {
		ident, ok := pathHead(n)
		if !ok {
			return nil
		}
		got, err := ident.GetAsString()
		if err != nil || !strings.EqualFold(got, name) {
			return nil
		}
		from, to, ok := nodeByteRange(ident)
		if !ok {
			return nil
		}
		refs = append(refs, scriptVariableRef{name: got, from: from, to: to})
		return nil
	})
	return refs
}

// byteOffsetLineCol returns the 1-based line of offset in text, and
// its column counted two ways: in bytes, and in characters with tabs
// expanded to the next multiple of 8 (the convention of GoogleSQL
// error locations).
func byteOffsetLineCol(text string, offset int) (line, byteCol, charCol int) {
	line = 1
	lineStart := 0
	for i := 0; i < offset && i < len(text); i++ {
		if text[i] == '\n' {
			line++
			lineStart = i + 1
		}
	}
	byteCol = offset - lineStart + 1
	charCol = 1
	for i := lineStart; i < offset && i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == '\t' {
			charCol += 8 - (charCol-1)%8
		} else {
			charCol++
		}
		i += size
	}
	return line, byteCol, charCol
}

// pickScriptVariableRef chooses the reference an analyzer error at
// line:col points to.
func pickScriptVariableRef(refs []scriptVariableRef, text string, line, col int) (scriptVariableRef, bool) {
	if len(refs) == 1 {
		return refs[0], true
	}
	for _, ref := range refs {
		l, bc, cc := byteOffsetLineCol(text, ref.from)
		if l == line && (bc == col || cc == col) {
			return ref, true
		}
	}
	return scriptVariableRef{}, false
}

// hasScriptVariableRefs reports whether stmt contains a path expression
// whose head is named after a declared script variable.
func hasScriptVariableRefs(conn *Conn, stmt googlesql.ASTNode) bool {
	if conn == nil || len(conn.scriptVars) == 0 {
		return false
	}
	found := false
	_ = astWalk(stmt, func(n googlesql.ASTNode) error {
		if found {
			return nil
		}
		ident, ok := pathHead(n)
		if !ok {
			return nil
		}
		got, err := ident.GetAsString()
		if err != nil {
			return nil
		}
		if _, ok := conn.ScriptVariable(got); ok {
			found = true
		}
		return nil
	})
	return found
}

// parseSingleStatement parses text, which must hold one statement.
func (a *Analyzer) parseSingleStatement(text string) (*parsedScript, googlesql.ASTStatementNode, error) {
	parsed, err := a.parseScript(text)
	if err != nil {
		return nil, nil, err
	}
	if len(parsed.stmts) != 1 {
		return nil, nil, fmt.Errorf("expected a single statement, got %d", len(parsed.stmts))
	}
	return parsed, parsed.stmts[0], nil
}

// resolvedStatement is a statement analyzed with its script variable
// references resolved. When variables were substituted, stmt and sql
// are the rewritten statement and its text, and parsed keeps its
// parser output alive.
type resolvedStatement struct {
	node   googlesql.ResolvedStatementNode
	stmt   googlesql.ASTStatementNode
	sql    string
	parsed *parsedScript
}

// analyzeResolvingScriptVariables analyzes stmt, replacing references
// to script variables the analyzer cannot resolve otherwise.
func (a *Analyzer) analyzeResolvingScriptVariables(conn *Conn, stmt googlesql.ASTStatementNode, mode googlesql.ParameterMode, args []driver.NamedValue, query string) (*resolvedStatement, error) {
	node, err := a.analyzeStatementLocked(stmt, mode, args, query)
	if err == nil {
		return &resolvedStatement{node: node, stmt: stmt, sql: query}, nil
	}
	if !hasScriptVariableRefs(conn, stmt) {
		return nil, err
	}
	origErr := err
	text := statementText(stmt, query)
	parsed, cur, perr := a.parseSingleStatement(text)
	if perr != nil {
		return nil, origErr
	}
	// Locations in err are relative to query; analyze the statement
	// alone so they become relative to text.
	if node, err = a.analyzeStatementLocked(cur, mode, args, text); err == nil {
		return &resolvedStatement{node: node, stmt: cur, sql: text, parsed: parsed}, nil
	}
	for range maxScriptVariableSubstitutions {
		name, line, col, ok := parseUnrecognizedName(err)
		if !ok {
			break
		}
		lit, declared := conn.ScriptVariable(name)
		if !declared {
			break
		}
		ref, ok := pickScriptVariableRef(collectScriptVariableRefs(cur, name), text, line, col)
		if !ok || ref.to > len(text) {
			break
		}
		text = text[:ref.from] + "(" + lit + ")" + text[ref.to:]
		parsed, cur, perr = a.parseSingleStatement(text)
		if perr != nil {
			return nil, origErr
		}
		node, err = a.analyzeStatementLocked(cur, mode, args, text)
		if err == nil {
			return &resolvedStatement{node: node, stmt: cur, sql: text, parsed: parsed}, nil
		}
	}
	if _, _, _, ok := parseUnrecognizedName(err); ok {
		return nil, err
	}
	// The analyzer rejected the statement for another reason, which can
	// happen when a variable sits where the analyzer wants a literal
	// (e.g. some OPTIONS values). Fall back to replacing every remaining
	// variable reference; keep the analyzer's error if that fails too.
	if fallback, ferr := a.analyzeSubstitutingAllScriptVariables(conn, cur, mode, args, text); ferr == nil {
		return fallback, nil
	}
	return nil, err
}

// analyzeSubstitutingAllScriptVariables replaces every path expression
// head naming a declared script variable in stmt and analyzes the
// result.
func (a *Analyzer) analyzeSubstitutingAllScriptVariables(conn *Conn, stmt googlesql.ASTStatementNode, mode googlesql.ParameterMode, args []driver.NamedValue, text string) (*resolvedStatement, error) {
	var refs []scriptVariableRef
	seen := map[string]bool{}
	_ = astWalk(stmt, func(n googlesql.ASTNode) error {
		ident, ok := pathHead(n)
		if !ok {
			return nil
		}
		got, err := ident.GetAsString()
		if err != nil || seen[strings.ToLower(got)] {
			return nil
		}
		if _, ok := conn.ScriptVariable(got); ok {
			seen[strings.ToLower(got)] = true
			refs = append(refs, collectScriptVariableRefs(stmt, got)...)
		}
		return nil
	})
	if len(refs) == 0 {
		return nil, fmt.Errorf("no script variable references")
	}
	// Replace from the end so earlier offsets stay valid.
	for i := 0; i < len(refs); i++ {
		for j := i + 1; j < len(refs); j++ {
			if refs[j].from > refs[i].from {
				refs[i], refs[j] = refs[j], refs[i]
			}
		}
	}
	for _, ref := range refs {
		if ref.to > len(text) {
			return nil, fmt.Errorf("script variable reference out of range")
		}
		lit, _ := conn.ScriptVariable(ref.name)
		text = text[:ref.from] + "(" + lit + ")" + text[ref.to:]
	}
	parsed, cur, err := a.parseSingleStatement(text)
	if err != nil {
		return nil, err
	}
	node, err := a.analyzeStatementLocked(cur, mode, args, text)
	if err != nil {
		return nil, err
	}
	return &resolvedStatement{node: node, stmt: cur, sql: text, parsed: parsed}, nil
}

// scriptVariableStmtAction runs a DECLARE or `SET name = expr`
// statement. The work happens when the script reaches the statement.
type scriptVariableStmtAction struct {
	run func(ctx context.Context, conn *Conn) error
	// prepare runs the statement in a prepared script, which has no
	// query arguments yet. nil means run.
	prepare func(ctx context.Context, conn *Conn) error
	done    bool
}

func (a *scriptVariableStmtAction) exec(ctx context.Context, conn *Conn) error {
	if a.done {
		return nil
	}
	if err := a.run(ctx, conn); err != nil {
		return err
	}
	a.done = true
	return nil
}

// Prepare runs the statement too: a prepared script still has to see
// the variable when its later statements are analyzed.
func (a *scriptVariableStmtAction) Prepare(ctx context.Context, conn *Conn) (driver.Stmt, error) {
	if a.prepare == nil || a.done {
		return nil, a.exec(ctx, conn)
	}
	if err := a.prepare(ctx, conn); err != nil {
		return nil, err
	}
	a.done = true
	return nil, nil
}

// scriptPrepareFunc returns how a DECLARE / SET statement whose
// expression uses query parameters runs in a prepared script, or nil
// when it uses none. A prepared statement binds its arguments to the
// last statement of the script only, so the expression cannot be
// evaluated: a named parameter is kept as text in the variable (cast to
// typeSQL when set) and binds where the variable is used. A positional
// parameter would bind out of order there, so it is rejected; so are
// parameters in `SET (...) =`, whose fields cannot be split as text.
func scriptPrepareFunc(stmt googlesql.ASTStatementNode, names []string, exprSQL, typeSQL string, splittable bool) func(context.Context, *Conn) error {
	positional := countPositionalParams(stmt)
	named := len(collectNamedParamNames(stmt))
	if positional == 0 && named == 0 {
		return nil
	}
	return func(_ context.Context, conn *Conn) error {
		if positional > 0 {
			return fmt.Errorf("not enough query arguments: positional parameters in DECLARE or SET are not supported in prepared statements")
		}
		if !splittable {
			return fmt.Errorf("not enough query arguments: query parameters in SET (...) are not supported in prepared statements")
		}
		text := "(" + exprSQL + ")"
		if typeSQL != "" {
			text = "CAST(" + text + " AS " + typeSQL + ")"
		}
		for _, name := range names {
			conn.SetScriptVariable(name, text)
		}
		return nil
	}
}

func (a *scriptVariableStmtAction) ExecContext(ctx context.Context, conn *Conn) (driver.Result, error) {
	if err := a.exec(ctx, conn); err != nil {
		return nil, err
	}
	return &Result{conn: conn}, nil
}

func (a *scriptVariableStmtAction) QueryContext(ctx context.Context, conn *Conn) (*Rows, error) {
	if err := a.exec(ctx, conn); err != nil {
		return nil, err
	}
	return &Rows{conn: conn}, nil
}

func (a *scriptVariableStmtAction) Args() []any { return nil }

func (a *scriptVariableStmtAction) Cleanup(context.Context, *Conn) error { return nil }

// scriptEnv is what a DECLARE / SET statement needs to evaluate its
// expression like any other statement of the script.
type scriptEnv struct {
	funcMap map[string]*FunctionSpec
	tvfMap  map[string]*TVFSpec
	args    []driver.NamedValue
}

// newDeclareAction builds the action of a DECLARE statement. declared
// holds the variables already declared by the script, so declaring one
// twice is an error, as in GoogleSQL.
func (a *Analyzer) newDeclareAction(decl *googlesql.ASTVariableDeclaration, query string, declared map[string]bool, env scriptEnv) (*scriptVariableStmtAction, error) {
	list, err := decl.VariableList()
	if err != nil || list == nil {
		return nil, fmt.Errorf("DECLARE: failed to read the variable list: %w", err)
	}
	num, err := list.NumChildren()
	if err != nil || num <= 0 {
		return nil, fmt.Errorf("DECLARE: no variable name")
	}
	names := make([]string, 0, num)
	for i := range num {
		ident, err := list.IdentifierList(i)
		if err != nil || ident == nil {
			return nil, fmt.Errorf("DECLARE: failed to read variable name: %w", err)
		}
		name, err := ident.GetAsString()
		if err != nil {
			return nil, fmt.Errorf("DECLARE: failed to read variable name: %w", err)
		}
		key := strings.ToLower(name)
		if declared[key] {
			return nil, fmt.Errorf("Variable %s is already declared", name)
		}
		declared[key] = true
		names = append(names, name)
	}
	var typeSQL, exprSQL string
	if typ, err := decl.Type(); err == nil && typ != nil {
		if typeSQL, _ = nodeText(typ, query); typeSQL == "" {
			return nil, fmt.Errorf("DECLARE: failed to read the variable type")
		}
	}
	if expr, err := decl.DefaultValue(); err == nil && expr != nil {
		if exprSQL, _ = nodeText(expr, query); exprSQL == "" {
			return nil, fmt.Errorf("DECLARE: failed to read the DEFAULT expression")
		}
	}
	return &scriptVariableStmtAction{prepare: scriptPrepareFunc(decl, names, exprSQL, typeSQL, true), run: func(ctx context.Context, conn *Conn) error {
		lit := "NULL"
		if exprSQL != "" || typeSQL != "" {
			var err error
			lit, err = a.evalScriptVariableExpr(ctx, conn, exprSQL, typeSQL, env)
			if err != nil {
				return fmt.Errorf("DECLARE %s: %w", strings.Join(names, ", "), err)
			}
		}
		for _, name := range names {
			conn.SetScriptVariable(name, lit)
		}
		return nil
	}}, nil
}

// newAssignmentAction builds the action of `SET name = expr`.
func (a *Analyzer) newAssignmentAction(asg *googlesql.ASTSingleAssignment, query string, env scriptEnv) (*scriptVariableStmtAction, error) {
	ident, err := asg.Variable()
	if err != nil || ident == nil {
		return nil, fmt.Errorf("SET: failed to read the variable name: %w", err)
	}
	name, err := ident.GetAsString()
	if err != nil {
		return nil, fmt.Errorf("SET: failed to read the variable name: %w", err)
	}
	expr, err := asg.Expression()
	if err != nil || expr == nil {
		return nil, fmt.Errorf("SET %s: failed to read the expression: %w", name, err)
	}
	exprSQL, ok := nodeText(expr, query)
	if !ok || exprSQL == "" {
		return nil, fmt.Errorf("SET %s: failed to read the expression", name)
	}
	action := &scriptVariableStmtAction{run: func(ctx context.Context, conn *Conn) error {
		cur, declared := conn.ScriptVariable(name)
		if !declared {
			return fmt.Errorf("Undeclared variable: %s", name)
		}
		// The value keeps the type of the variable.
		typeSQL := scriptLiteralType(cur)
		lit, err := a.evalScriptVariableExpr(ctx, conn, exprSQL, typeSQL, env)
		if err != nil {
			return fmt.Errorf("SET %s: %w", name, err)
		}
		conn.SetScriptVariable(name, lit)
		return nil
	}}
	if prepare := scriptPrepareFunc(asg, []string{name}, exprSQL, "", true); prepare != nil {
		action.prepare = func(ctx context.Context, conn *Conn) error {
			cur, declared := conn.ScriptVariable(name)
			if !declared {
				return fmt.Errorf("Undeclared variable: %s", name)
			}
			return scriptPrepareFunc(asg, []string{name}, exprSQL, scriptLiteralType(cur), true)(ctx, conn)
		}
	}
	return action, nil
}

// scriptLiteralType returns the type of a literal stored by
// evalScriptVariableExpr, or "" when it is untyped.
func scriptLiteralType(lit string) string {
	const prefix = "CAST("
	if !strings.HasPrefix(lit, prefix) || !strings.HasSuffix(lit, ")") {
		return ""
	}
	// The type follows the last top-level " AS ".
	idx := strings.LastIndex(lit, " AS ")
	if idx < 0 {
		return ""
	}
	return strings.TrimSuffix(lit[idx+len(" AS "):], ")")
}

// newStructAssignmentAction builds the action of
// `SET (name, ...) = struct_expr`: field i of the struct goes to the
// i-th variable.
func (a *Analyzer) newStructAssignmentAction(asg *googlesql.ASTAssignmentFromStruct, query string, env scriptEnv) (*scriptVariableStmtAction, error) {
	list, err := asg.Variables()
	if err != nil || list == nil {
		return nil, fmt.Errorf("SET: failed to read the variable list: %w", err)
	}
	num, err := list.NumChildren()
	if err != nil || num <= 0 {
		return nil, fmt.Errorf("SET: no variable name")
	}
	names := make([]string, 0, num)
	seen := map[string]bool{}
	for i := range num {
		ident, err := list.IdentifierList(i)
		if err != nil || ident == nil {
			return nil, fmt.Errorf("SET: failed to read variable name: %w", err)
		}
		name, err := ident.GetAsString()
		if err != nil {
			return nil, fmt.Errorf("SET: failed to read variable name: %w", err)
		}
		if seen[strings.ToLower(name)] {
			return nil, fmt.Errorf("SET: variable %s is assigned more than once", name)
		}
		seen[strings.ToLower(name)] = true
		names = append(names, name)
	}
	expr, err := asg.StructExpression()
	if err != nil || expr == nil {
		return nil, fmt.Errorf("SET: failed to read the expression: %w", err)
	}
	exprSQL, ok := nodeText(expr, query)
	if !ok || exprSQL == "" {
		return nil, fmt.Errorf("SET: failed to read the expression")
	}
	return &scriptVariableStmtAction{prepare: scriptPrepareFunc(asg, names, exprSQL, "", false), run: func(ctx context.Context, conn *Conn) error {
		types := make([]string, len(names))
		for i, name := range names {
			cur, declared := conn.ScriptVariable(name)
			if !declared {
				return fmt.Errorf("Undeclared variable: %s", name)
			}
			types[i] = scriptLiteralType(cur)
		}
		val, typ, err := a.evalScriptValue(ctx, conn, "SELECT ("+exprSQL+")", env)
		if err != nil {
			return fmt.Errorf("SET: %w", err)
		}
		if !typ.IsStruct() || len(typ.FieldTypes) != len(names) {
			return fmt.Errorf("SET: expected a STRUCT with %d fields, got %s", len(names), typ.FormatType())
		}
		var fields []value.Value
		if val != nil {
			st, err := val.ToStruct()
			if err != nil {
				return fmt.Errorf("SET: %w", err)
			}
			fields = st.Values
		}
		lits := make([]string, len(names))
		for i := range names {
			var field value.Value
			if i < len(fields) {
				field = fields[i]
			}
			fieldType := typ.FieldTypes[i].Type
			lit, err := scriptLiteral(field, fieldType)
			if err != nil {
				return fmt.Errorf("SET %s: %w", names[i], err)
			}
			if fieldTypeSQL, _ := scriptTypeSQL(fieldType); types[i] != "" && types[i] != fieldTypeSQL {
				// Coerce to the type of the variable.
				if lit, err = a.evalScriptVariableExpr(ctx, conn, lit, types[i], scriptEnv{funcMap: env.funcMap, tvfMap: env.tvfMap}); err != nil {
					return fmt.Errorf("SET %s: %w", names[i], err)
				}
			}
			lits[i] = lit
		}
		for i, name := range names {
			conn.SetScriptVariable(name, lits[i])
		}
		return nil
	}}, nil
}

// evalScriptVariableExpr evaluates expr (coerced to typeSQL when set)
// through the regular query path and returns its value as a typed
// GoogleSQL literal.
func (a *Analyzer) evalScriptVariableExpr(ctx context.Context, conn *Conn, exprSQL, typeSQL string, env scriptEnv) (string, error) {
	var sel string
	switch {
	case exprSQL == "":
		sel = "SELECT CAST(NULL AS " + typeSQL + ")"
	case typeSQL != "":
		sel = "SELECT CAST((" + exprSQL + ") AS " + typeSQL + ")"
	default:
		sel = "SELECT (" + exprSQL + ")"
	}
	val, typ, err := a.evalScriptValue(ctx, conn, sel, env)
	if err != nil {
		return "", err
	}
	return scriptLiteral(val, typ)
}

// evalScriptValue runs sel, a query returning one value, through the
// regular query path and returns the value and its type.
func (a *Analyzer) evalScriptValue(ctx context.Context, conn *Conn, sel string, env scriptEnv) (value.Value, *Type, error) {
	parsed, stmt, err := a.parseSingleStatement(sel)
	if err != nil {
		return nil, nil, err
	}
	mode, err := a.getParameterMode(stmt)
	if err != nil {
		return nil, nil, err
	}
	resolved, err := a.analyzeResolvingScriptVariables(conn, stmt, mode, env.args, sel)
	if err != nil {
		return nil, nil, err
	}
	defer runtime.KeepAlive(parsed)
	defer runtime.KeepAlive(resolved.parsed)
	queryNode, ok := resolved.node.(*googlesql.ResolvedQueryStmt)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected statement %T", resolved.node)
	}
	qctx := a.context(ctx, env.funcMap, env.tvfMap)
	qctx = withSystemVars(qctx, conn.systemVars)
	qctx = withConn(qctx, conn)
	action, err := a.newQueryStmtAction(qctx, resolved.sql, env.args, queryNode)
	if err != nil {
		return nil, nil, err
	}
	if len(action.outputColumns) != 1 {
		return nil, nil, fmt.Errorf("expected one value, got %d columns", len(action.outputColumns))
	}
	rows, err := conn.QueryContext(ctx, action.formattedQuery, action.args...)
	if err != nil {
		return nil, nil, unwrapSQLiteUserError(err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, nil, unwrapSQLiteUserError(err)
		}
		return nil, nil, fmt.Errorf("expression returned no row")
	}
	var raw any
	if err := rows.Scan(&raw); err != nil {
		return nil, nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, nil, unwrapSQLiteUserError(err)
	}
	val, err := DecodeValue(raw)
	if err != nil {
		return nil, nil, err
	}
	return val, action.outputColumns[0].Type, nil
}

// scriptTypeSQL returns the GoogleSQL name of t.
func scriptTypeSQL(t *Type) (string, bool) {
	if t == nil {
		return "", false
	}
	switch t.kindAs() {
	case googlesql.TypeKindTypeArray:
		elem, ok := scriptTypeSQL(t.ElementType)
		if !ok {
			return "", false
		}
		return "ARRAY<" + elem + ">", true
	case googlesql.TypeKindTypeStruct:
		fields := make([]string, 0, len(t.FieldTypes))
		for _, f := range t.FieldTypes {
			ft, ok := scriptTypeSQL(f.Type)
			if !ok {
				return "", false
			}
			if f.Name == "" {
				fields = append(fields, ft)
			} else {
				fields = append(fields, "`"+strings.ReplaceAll(f.Name, "`", "\\`")+"` "+ft)
			}
		}
		return "STRUCT<" + strings.Join(fields, ", ") + ">", true
	case googlesql.TypeKindTypeRange:
		elem, ok := scriptTypeSQL(t.ElementType)
		if !ok {
			return "", false
		}
		return "RANGE<" + elem + ">", true
	}
	name := typeKindToSQLName(t.kindAs())
	if name == "" || strings.HasPrefix(name, "TypeKind") {
		return "", false
	}
	return name, true
}

// scriptLiteral formats v, a value of type t, as a GoogleSQL literal
// expression of that type.
func scriptLiteral(v value.Value, t *Type) (string, error) {
	typeSQL, ok := scriptTypeSQL(t)
	if !ok {
		return "", fmt.Errorf("unsupported script variable type %s", t.FormatType())
	}
	body, err := scriptLiteralBody(v, t)
	if err != nil {
		return "", err
	}
	return "CAST(" + body + " AS " + typeSQL + ")", nil
}

// scriptLiteralBody formats v as an expression that CAST(... AS t)
// turns into the value.
func scriptLiteralBody(v value.Value, t *Type) (string, error) {
	if v == nil {
		return "NULL", nil
	}
	switch t.kindAs() {
	case googlesql.TypeKindTypeArray:
		arr, err := v.ToArray()
		if err != nil {
			return "", err
		}
		elemType, ok := scriptTypeSQL(t.ElementType)
		if !ok {
			return "", fmt.Errorf("unsupported array element type")
		}
		elems := make([]string, 0, len(arr.Values))
		for _, e := range arr.Values {
			s, err := scriptLiteral(e, t.ElementType)
			if err != nil {
				return "", err
			}
			elems = append(elems, s)
		}
		return "ARRAY<" + elemType + ">[" + strings.Join(elems, ", ") + "]", nil
	case googlesql.TypeKindTypeStruct:
		st, err := v.ToStruct()
		if err != nil {
			return "", err
		}
		if len(st.Values) != len(t.FieldTypes) {
			return "", fmt.Errorf("struct value has %d fields, type has %d", len(st.Values), len(t.FieldTypes))
		}
		typeSQL, _ := scriptTypeSQL(t)
		fields := make([]string, 0, len(st.Values))
		for i, f := range st.Values {
			s, err := scriptLiteral(f, t.FieldTypes[i].Type)
			if err != nil {
				return "", err
			}
			fields = append(fields, s)
		}
		return typeSQL + "(" + strings.Join(fields, ", ") + ")", nil
	case googlesql.TypeKindTypeBool:
		b, err := v.ToBool()
		if err != nil {
			return "", err
		}
		if b {
			return "TRUE", nil
		}
		return "FALSE", nil
	case googlesql.TypeKindTypeInt32, googlesql.TypeKindTypeInt64,
		googlesql.TypeKindTypeUint32, googlesql.TypeKindTypeUint64:
		i, err := v.ToInt64()
		if err != nil {
			return "", err
		}
		return strconv.FormatInt(i, 10), nil
	case googlesql.TypeKindTypeFloat, googlesql.TypeKindTypeDouble:
		f, err := v.ToFloat64()
		if err != nil {
			return "", err
		}
		switch {
		case math.IsNaN(f):
			return `"nan"`, nil
		case math.IsInf(f, 1):
			return `"inf"`, nil
		case math.IsInf(f, -1):
			return `"-inf"`, nil
		}
		return strconv.Quote(strconv.FormatFloat(f, 'g', -1, 64)), nil
	case googlesql.TypeKindTypeBytes:
		b, err := v.ToBytes()
		if err != nil {
			return "", err
		}
		return value.BytesValue(b).Format('T'), nil
	case googlesql.TypeKindTypeJson:
		s, err := v.ToJSON()
		if err != nil {
			return "", err
		}
		return "JSON " + strconv.Quote(s), nil
	case googlesql.TypeKindTypeTimestamp:
		ts, err := v.ToTime()
		if err != nil {
			return "", err
		}
		return strconv.Quote(ts.UTC().Format("2006-01-02 15:04:05.999999+00")), nil
	case googlesql.TypeKindTypeDate, googlesql.TypeKindTypeDatetime,
		googlesql.TypeKindTypeTime:
		return v.Format('T'), nil
	case googlesql.TypeKindTypeGeography:
		wkt, err := v.ToString()
		if err != nil {
			return "", err
		}
		return "ST_GEOGFROMTEXT(" + strconv.Quote(wkt) + ")", nil
	}
	// STRING, NUMERIC, BIGNUMERIC, INTERVAL and anything else CAST can
	// read back from its string form.
	s, err := v.ToString()
	if err != nil {
		return "", err
	}
	return strconv.Quote(s), nil
}
