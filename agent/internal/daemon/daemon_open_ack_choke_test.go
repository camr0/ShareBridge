package daemon

// M4 remediation round C, fix round 3: the structural half of the OK open_ack
// guard, strengthened from the fix-round-2 version.
//
// Three separate bypasses of the direct-open success path were found in this
// remediation (the cold open, the already-open non-renewal fast path, and the
// endpoint-report confirmation path). Round 2 answered them with a single
// *conventional* OK-ack writer plus an AST guard over daemon.go. That guard
// pinned today's shape, but not the transport: signaling.OpenAck is publicly
// constructible and Client.OpenAck is publicly callable, so a new caller in
// another file could emit an unvalidated OK ack.
//
// Fix round 3 seals the transport (see Daemon.openAckGuard and
// signaling.Client.checkOpenAckSeal) and this test makes the static half
// robust:
//
//   - it scans the WHOLE production agent module (not just daemon.go) for
//     signaling.OpenAck constructions that carry an OK status, including
//     assignment-form `X.Status = "ok"` and non-literal (variable/const)
//     Status values that could be "ok" at run time;
//   - it requires exactly one such construction, inside ackOpenSuccess, and
//     requires that writer to call CommitOpenAck, to derive the granted port
//     from the commit, and to attach the validation context the transport guard
//     consumes;
//   - it requires the production daemon to install the guard at construction.
//
// What it does NOT guarantee, and why the transport seal makes the gap
// non-exploitable: a hand-rolled map (`map[string]any{"type":"open_ack",
// "status":"ok"}`) or a status computed by an opaque helper cannot be classified
// statically. Both hand the transport a message with no recoverable validation
// context, so signaling.Client.Send refuses it; and any caller that supplies a
// genuinely valid context has, by construction, performed the same
// CommitOpenAck generation re-validation the choke point performs. The static
// scan is therefore a *source-level* tripwire for new writers; the transport is
// the guarantee.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// okAckSite is one production source construction that can produce a
// status-"ok" open_ack.
type okAckSite struct {
	pos token.Position
	lit *ast.CompositeLit
}

// TestOpenAckOKOnlyInsideTheValidatedChokePoint is the module-wide structural
// guard. It fails on a second OK-ack writer anywhere in production code, on a
// writer that skips the port's re-validation or the validation context, on an
// assignment-form or unclassifiable OK status, and on a daemon that fails to
// install the transport guard.
func TestOpenAckOKOnlyInsideTheValidatedChokePoint(t *testing.T) {
	root := agentModuleRoot(t)
	fset := token.NewFileSet()

	var (
		okSites        []okAckSite
		unverifiable   []string
		assignmentForm []string
		dotImports     []string
	)

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		importsSignaling := false
		signalingName := "signaling"
		for _, imp := range file.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if importPath == "sharebridge/agent/internal/signaling" || strings.HasSuffix(importPath, "/signaling") {
				importsSignaling = true
				if imp.Name != nil {
					if imp.Name.Name == "." {
						dotImports = append(dotImports, fset.Position(imp.Pos()).String())
					} else if imp.Name.Name != "_" {
						signalingName = imp.Name.Name
					}
				}
			}
		}
		for _, lit := range signalingOpenAckLiterals(file, signalingName) {
			kind := openAckStatusKind(lit)
			switch kind {
			case "ok":
				okSites = append(okSites, okAckSite{pos: fset.Position(lit.Pos()), lit: lit})
			case "nonliteral":
				unverifiable = append(unverifiable, fset.Position(lit.Pos()).String())
			}
		}
		if importsSignaling {
			assignmentForm = append(assignmentForm, statusOkAssignments(fset, file)...)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk agent module %s: %v", root, err)
	}

	if len(dotImports) != 0 {
		t.Fatalf("dot-import of the signaling package hides the qualifier from the structural scan: %v", dotImports)
	}
	if len(unverifiable) != 0 {
		t.Fatalf("signaling.OpenAck construction with a non-literal Status (could be \"ok\" at run time): %v", unverifiable)
	}
	if len(assignmentForm) != 0 {
		t.Fatalf("assignment-form status \"ok\" in a signaling-importing file: %v; "+
			"an OK open_ack must be built by the single writer with a validation context", assignmentForm)
	}
	if len(okSites) != 1 {
		t.Fatalf("the production agent module constructs %d status-\"ok\" signaling.OpenAck values, want exactly 1 "+
			"(inside ackOpenSuccess); a second one is a success path that can skip the generation re-validation", len(okSites))
	}

	// The one OK construction must live inside ackOpenSuccess (in daemon.go).
	daemonFile, err := parser.ParseFile(fset, filepath.Join(root, "internal", "daemon", "daemon.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse daemon.go: %v", err)
	}
	ackFn := findFunc(daemonFile, "ackOpenSuccess")
	if ackFn == nil {
		t.Fatalf("daemon.go has no ackOpenSuccess function: the OK open_ack choke point that " +
			"re-validates the generation is missing")
	}
	if !sameFile(okSites[0].pos, fset.Position(ackFn.Pos())) {
		t.Fatalf("the only status-\"ok\" signaling.OpenAck is at %s, outside daemon.go's ackOpenSuccess",
			okSites[0].pos)
	}
	if okSites[0].pos.Offset < fset.Position(ackFn.Pos()).Offset || okSites[0].pos.Offset > fset.Position(ackFn.End()).Offset {
		t.Fatalf("the only status-\"ok\" signaling.OpenAck is at %s, outside ackOpenSuccess (%s..%s)",
			okSites[0].pos, fset.Position(ackFn.Pos()), fset.Position(ackFn.End()))
	}

	// ackOpenSuccess must re-validate internally: it must call CommitOpenAck.
	if !callsCommitOpenAck(ackFn) {
		t.Fatalf("ackOpenSuccess does not call CommitOpenAck: the sole OK open_ack writer must " +
			"re-validate the §13.4 generation on the port's state loop immediately before emitting")
	}
	// ... and must not be handed a pre-validated token by a caller: the
	// authorization is produced inside the writer, after the report wait.
	if paramTypeOf(ackFn, "OpenAckCommit") {
		t.Fatalf("ackOpenSuccess takes an OpenAckCommit parameter: a caller could validate the " +
			"generation BEFORE the endpoint-report wait and then ack after it (the A1 bypass)")
	}
	// The ack's granted port must come from that re-validation, not from a
	// value read before the wait.
	if !grantedPortFromCommit(okSites[0].lit) {
		t.Fatalf("the OK open_ack's GrantedPort is not derived from the commit token: the port the " +
			"ack advertises must be the one CommitOpenAck re-confirmed")
	}
	// ... and it must carry the validation context the transport guard consumes,
	// or the sealed transport would refuse the daemon's own OK acks.
	if !validationContextSet(okSites[0].lit) {
		t.Fatalf("the OK open_ack does not set Validation: the transport guard has no generation to " +
			"re-validate and would refuse to send it")
	}

	// The production daemon must install the transport guard at construction.
	if !installsOpenAckGuard(daemonFile) {
		t.Fatalf("daemon.go never calls SetOpenAckGuard: the transport seal is not wired, so an OK " +
			"ack would fail closed (or, if the seal were removed, an unvalidated one could be sent)")
	}
}

// agentModuleRoot walks up from the test's working directory to the agent
// module root (the directory containing go.mod).
func agentModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above the test working directory")
		}
		dir = parent
	}
}

// signalingOpenAckLiterals returns every composite literal of type
// <name>.OpenAck in file, where name is the local name of the signaling
// package (default or aliased).
func signalingOpenAckLiterals(file *ast.File, name string) []*ast.CompositeLit {
	var lits []*ast.CompositeLit
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != name || sel.Sel.Name != "OpenAck" {
			return true
		}
		lits = append(lits, lit)
		return true
	})
	return lits
}

// openAckStatusKind classifies a signaling.OpenAck literal's Status field:
// "ok" (a literal "ok"), "other" (a different literal, e.g. "error"), "absent"
// (zero value, which control ignores), or "nonliteral" (a variable/const/expr
// that could be "ok" at run time).
func openAckStatusKind(lit *ast.CompositeLit) string {
	found := false
	kind := "absent"
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Status" {
			continue
		}
		found = true
		bl, ok := kv.Value.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			kind = "nonliteral"
			continue
		}
		if bl.Value == `"ok"` {
			kind = "ok"
		} else {
			kind = "other"
		}
	}
	if !found {
		return "absent"
	}
	return kind
}

// statusOkAssignments returns the positions of `X.Status = "ok"` assignments in
// file. This is the assignment-form bypass the round-2 literal-only scan missed.
func statusOkAssignments(fset *token.FileSet, file *ast.File) []string {
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || assign.Tok != token.ASSIGN {
			return true
		}
		for i, lhs := range assign.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Status" || i >= len(assign.Rhs) {
				continue
			}
			bl, ok := assign.Rhs[i].(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING || bl.Value != `"ok"` {
				continue
			}
			found = append(found, fset.Position(assign.Pos()).String())
		}
		return true
	})
	return found
}

// findFunc returns the named function declaration in file, or nil.
func findFunc(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// sameFile reports whether two positions name the same source file.
func sameFile(a, b token.Position) bool { return a.Filename == b.Filename }

// callsCommitOpenAck reports whether fn's body contains a call to a
// CommitOpenAck method (the port's post-open generation re-validation).
func callsCommitOpenAck(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if isSel && sel.Sel.Name == "CommitOpenAck" {
			found = true
			return false
		}
		return true
	})
	return found
}

// paramTypeOf reports whether any parameter of fn names the given type (matched
// on the selector's type name, e.g. direct.OpenAckCommit).
func paramTypeOf(fn *ast.FuncDecl, typeName string) bool {
	if fn.Type == nil || fn.Type.Params == nil {
		return false
	}
	for _, field := range fn.Type.Params.List {
		switch t := field.Type.(type) {
		case *ast.SelectorExpr:
			if t.Sel.Name == typeName {
				return true
			}
		case *ast.StarExpr:
			if sel, ok := t.X.(*ast.SelectorExpr); ok && sel.Sel.Name == typeName {
				return true
			}
		}
	}
	return false
}

// grantedPortFromCommit reports whether the OK OpenAck literal's GrantedPort
// value is a call to GrantedPort() (the commit token accessor).
func grantedPortFromCommit(okAck *ast.CompositeLit) bool {
	for _, elt := range okAck.Elts {
		kv, isKV := elt.(*ast.KeyValueExpr)
		if !isKV {
			continue
		}
		key, isKey := kv.Key.(*ast.Ident)
		if !isKey || key.Name != "GrantedPort" {
			continue
		}
		return isGrantedPortCall(kv.Value)
	}
	return false
}

// isGrantedPortCall reports whether expr is a call to a GrantedPort() method.
func isGrantedPortCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "GrantedPort"
}

// validationContextSet reports whether the OK OpenAck literal sets Validation to
// a non-nil signaling.OpenAckValidation value (the transport guard's context).
func validationContextSet(okAck *ast.CompositeLit) bool {
	for _, elt := range okAck.Elts {
		kv, isKV := elt.(*ast.KeyValueExpr)
		if !isKV {
			continue
		}
		key, isKey := kv.Key.(*ast.Ident)
		if !isKey || key.Name != "Validation" {
			continue
		}
		unary, isUnary := kv.Value.(*ast.UnaryExpr)
		if !isUnary || unary.Op != token.AND {
			return false
		}
		lit, isLit := unary.X.(*ast.CompositeLit)
		if !isLit {
			return false
		}
		sel, isSel := lit.Type.(*ast.SelectorExpr)
		if !isSel {
			return false
		}
		pkg, isIdent := sel.X.(*ast.Ident)
		return isIdent && pkg.Name == "signaling" && sel.Sel.Name == "OpenAckValidation"
	}
	return false
}

// installsOpenAckGuard reports whether file calls SetOpenAckGuard (the daemon's
// construction-time wiring of the transport seal).
func installsOpenAckGuard(file *ast.File) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if ok && sel.Sel.Name == "SetOpenAckGuard" {
			found = true
			return false
		}
		return true
	})
	return found
}
