// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file contains test cases for short valid and invalid programs.

package parser

import (
	"gong/internal/typeparams"
	"testing"
)

var valids = []string{
	"package p\n",
	`package p;`,
	`package p; import "fmt"; fun f() { fmt.Println("Hello, World!") };`,
	`package p; fun f() { if f(T()) {} };`,
	`package p; fun f(fun() fun() fun());`,
	`package p; fun f(...T);`,
	`package p; fun f(float, ...int);`,
	`package p; fun f(x int, a ...int) { f(0, a...); f(1, a...,) };`,
	`package p; fun f(int,) {};`,
	`package p; fun f(...int,) {};`,
	`package p; fun f(x ...int,) {};`,
	`package p; fun f() { if ; true {} };`,
	`package p; fun ((T),) m() {}`,
	`package p; fun ((*T),) m() {}`,
	`package p; fun (*(T),) m() {}`,
	`package p; const (x = 0; y; z)`,
	`package p; type T = int`,
	`package p; type T (*int)`,
	`package p; var _ = fun()T(nil)`,
	`package p; fun _(T (P))`,
}

// validWithTParamsOnly holds source code examples that are valid if
// parseTypeParams is set, but invalid if not. When checking with the
// parseTypeParams set, errors are ignored.
var validWithTParamsOnly = []string{
	`package p; fun _[ /* ERROR "expected '\(', found '\['" */ T any]()()`,
	`package p; fun _(T (P))`,
	`package p; fun f[ /* ERROR "expected '\(', found '\['" */ A, B any](); fun _() { _ = f[int, int] }`,
	`package p; fun _(x /* ERROR "mixed named and unnamed parameters" */ T[P1, P2, P3])`,
	`package p; fun _(x /* ERROR "mixed named and unnamed parameters" */ p.T[Q])`,
	`package p; fun _(p.T[ /* ERROR "missing ',' in parameter list" */ Q])`,
	`package p; fun _[ /* ERROR "expected '\(', found '\['" */ T any]()`,
	`package p; fun _[ /* ERROR "expected '\(', found '\['" */ T any](x T)`,
	`package p; fun _[ /* ERROR "expected '\(', found '\['" */ T1, T2 any](x T)`,
	`package p; fun _[ /* ERROR "expected '\(', found '\['" */ A, B any](a A) B`,
	`package p; fun _[ /* ERROR "expected '\(', found '\['" */ A, B C](a A) B`,
	`package p; fun _[ /* ERROR "expected '\(', found '\['" */ A, B C[A, B]](a A) B`,
	`package p; fun (T) _[ /* ERROR "expected '\(', found '\['" */ A, B any](a A) B`,
	`package p; fun (T) _[ /* ERROR "expected '\(', found '\['" */ A, B C](a A) B`,
	`package p; fun (T) _[ /* ERROR "expected '\(', found '\['" */ A, B C[A, B]](a A) B`,
	`package p; fun _(_ T[ /* ERROR "missing ',' in parameter list" */ P], T P) T[P]`,

	// TODO(rfindley) this error message could be improved.
	`package p; fun (_ /* ERROR "mixed named and unnamed parameters" */ R[P]) _[T any](x T)`,
	`package p; fun (_ /* ERROR "mixed named and unnamed parameters" */ R[ P, Q]) _[T1, T2 any](x T)`,

	`package p; fun (R[P] /* ERROR "missing element type" */ ) _[T any]()`,
	`package p; fun _(T[P] /* ERROR "missing element type" */ )`,
	`package p; fun _(T[P1, /* ERROR "expected ']', found ','" */ P2, P3 ])`,
	`package p; fun _(T[P] /* ERROR "missing element type" */ ) T[P]`,
}

func TestValid(t *testing.T) {
	t.Run("no tparams", func(t *testing.T) {
		for _, src := range valids {
			checkErrors(t, src, src, DeclarationErrors|AllErrors, false)
		}
	})
	t.Run("tparams", func(t *testing.T) {
		if !typeparams.Enabled {
			t.Skip("type params are not enabled")
		}
		for _, src := range valids {
			checkErrors(t, src, src, DeclarationErrors|AllErrors, false)
		}
		for _, src := range validWithTParamsOnly {
			checkErrors(t, src, src, DeclarationErrors|AllErrors, false)
		}
	})
}

// TestSingle is useful to track down a problem with a single short test program.
func TestSingle(t *testing.T) {
	const src = `package p; var _ = f()`
	checkErrors(t, src, src, DeclarationErrors|AllErrors, true)
}

var invalids = []string{
	`foo /* ERROR "expected 'package'" */ !`,
	`package p; fun f() { if { /* ERROR "missing condition" */ } };`,
	`package p; fun f() { if ; /* ERROR "missing condition" */ {} };`,
	`package p; fun f() { if f(); /* ERROR "missing condition" */ {} };`,
	`package p; var a = fun /* ERROR "expected expression" */ ();`,
	`package p; fun f() { if x := g(); x /* ERROR "expected boolean expression" */ = 0 {}};`,
	`package p; fun f() { _ = x = /* ERROR "expected '=='" */ 0 {}};`,
	`package p; fun f() { _ = 1 == fun()int { var x bool; x = x = /* ERROR "expected '=='" */ true; return x }() };`,
	`package p; fun _() (type /* ERROR "found 'type'" */ T)(T)`,
	`package p; fun (type /* ERROR "found 'type'" */ T)(T) _()`,

	`package p; fun f() (a b string /* ERROR "missing ','" */ , ok bool)`,

	`package p; var x /* ERROR "missing variable type or initialization" */ , y, z;`,
	`package p; const x /* ERROR "missing constant value" */ ;`,
	`package p; const x /* ERROR "missing constant value" */ int;`,
	`package p; const (x = 0; y; z /* ERROR "missing constant value" */ int);`,

	// issue 13475
	`package p; fun f() { if true {} else ; /* ERROR "expected if statement or block" */ }`,
}

// invalidNoTParamErrs holds invalid source code examples annotated with the
// error messages produced when ParseTypeParams is not set.
var invalidNoTParamErrs = []string{
	// `package p; type T[P any /* ERROR "expected ']', found any" */ ] = T0`,
	`package p; var _ fun[ /* ERROR "expected '\(', found '\['" */ T any](T)`,
	`package p; fun _[ /* ERROR "expected '\(', found '\['" */ ]()`,
}

// invalidTParamErrs holds invalid source code examples annotated with the
// error messages produced when ParseTypeParams is set.
var invalidTParamErrs = []string{
	`package p; type T[P any] = /* ERROR "cannot be alias" */ T0`,
	`package p; var _ fun[ /* ERROR "cannot have type parameters" */ T any](T)`,
	`package p; fun _[]/* ERROR "empty type parameter list" */()`,
}

func TestInvalid(t *testing.T) {
	t.Run("no tparams", func(t *testing.T) {
		for _, src := range invalids {
			checkErrors(t, src, src, DeclarationErrors|AllErrors|typeparams.DisallowParsing, true)
		}
		for _, src := range validWithTParamsOnly {
			checkErrors(t, src, src, DeclarationErrors|AllErrors|typeparams.DisallowParsing, true)
		}
		for _, src := range invalidNoTParamErrs {
			checkErrors(t, src, src, DeclarationErrors|AllErrors|typeparams.DisallowParsing, true)
		}
	})
	t.Run("tparams", func(t *testing.T) {
		if !typeparams.Enabled {
			t.Skip("type params are not enabled")
		}
		for _, src := range invalids {
			checkErrors(t, src, src, DeclarationErrors|AllErrors, true)
		}
		for _, src := range invalidTParamErrs {
			checkErrors(t, src, src, DeclarationErrors|AllErrors, true)
		}
	})
}
