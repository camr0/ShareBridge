package daemon

// M4 remediation round C, fix round: the structural guard for the OK open_ack
// choke point.
//
// Three separate bypasses of the direct-open success path have been found in
// this remediation (the cold open, the already-open non-renewal fast path, and
// the endpoint-report confirmation path). Each was a *new* code path that
// emitted a status-"ok" open_ack without re-validating the open's §13.4
// generation immediately before the emission. The behavioural test
// TestOpenSignalFencedByLockdownDuringReportConfirm pins the known
// interleaving; this test pins the STRUCTURE that makes the next one fail:
//
//   - the daemon may construct exactly ONE status-"ok" signaling.OpenAck
//     literal, and it must live inside ackOpenSuccess;
//   - ackOpenSuccess must itself call OnDemandPort.CommitOpenAck (the port's
//     generation re-validation, performed after the endpoint report was
//     confirmed); and
//   - ackOpenSuccess must not accept an OpenAckCommit parameter, i.e. it must
//     produce the authorization internally rather than being handed one a
//     caller validated earlier (the A1 bypass: validate before the report wait,
//     ack after it).
//
// A future contributor who adds another OK ack path — or who weakens
// ackOpenSuccess back into a token-taking writer — is caught here even if their
// new interleaving is not yet covered by a behavioural test.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestOpenAckOKOnlyInsideTheValidatedChokePoint parses the production daemon
// source and fails unless the single OK open_ack is constructed inside
// ackOpenSuccess, which must internally re-validate the generation. It is
// deliberately an AST check (not a string count) so formatting and helper
// extraction do not matter: what is forbidden is a second OK ack construction
// anywhere else in the package, and an OK ack writer that skips the port's
// re-validation.
func TestOpenAckOKOnlyInsideTheValidatedChokePoint(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "daemon.go", nil, 0)
	if err != nil {
		t.Fatalf("parse daemon.go: %v", err)
	}

	var okAcks []*ast.CompositeLit
	ast.Inspect(file, func(n ast.Node) bool {
		lit, isLit := n.(*ast.CompositeLit)
		if !isLit {
			return true
		}
		sel, isSel := lit.Type.(*ast.SelectorExpr)
		if !isSel {
			return true
		}
		pkg, isIdent := sel.X.(*ast.Ident)
		if !isIdent || pkg.Name != "signaling" || sel.Sel.Name != "OpenAck" {
			return true
		}
		for _, elt := range lit.Elts {
			kv, isKV := elt.(*ast.KeyValueExpr)
			if !isKV {
				continue
			}
			key, isKey := kv.Key.(*ast.Ident)
			if !isKey || key.Name != "Status" {
				continue
			}
			bl, isBasic := kv.Value.(*ast.BasicLit)
			if isBasic && bl.Kind == token.STRING && bl.Value == `"ok"` {
				okAcks = append(okAcks, lit)
			}
		}
		return true
	})

	var ackFn *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, isFn := decl.(*ast.FuncDecl)
		if isFn && fn.Name.Name == "ackOpenSuccess" {
			ackFn = fn
			break
		}
	}
	if ackFn == nil {
		t.Fatalf("daemon.go has no ackOpenSuccess function: the OK open_ack choke point that " +
			"re-validates the generation is missing")
	}
	if len(okAcks) != 1 {
		t.Fatalf("daemon.go constructs %d status-\"ok\" signaling.OpenAck literals, want exactly 1 "+
			"(inside ackOpenSuccess); a second one is a success path that can skip the generation "+
			"re-validation", len(okAcks))
	}
	okPos := okAcks[0].Pos()
	if okPos < ackFn.Pos() || okPos > ackFn.End() {
		t.Fatalf("the only status-\"ok\" signaling.OpenAck literal is at %s, outside ackOpenSuccess "+
			"(%s..%s): an OK ack is being emitted without the choke point",
			fset.Position(okPos), fset.Position(ackFn.Pos()), fset.Position(ackFn.End()))
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
	if !grantedPortFromCommit(okAcks[0]) {
		t.Fatalf("the OK open_ack's GrantedPort is not derived from the commit token: the port the " +
			"ack advertises must be the one CommitOpenAck re-confirmed")
	}
}

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
		call, isCall := kv.Value.(*ast.CallExpr)
		if !isCall {
			return false
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		return isSel && sel.Sel.Name == "GrantedPort"
	}
	return false
}
