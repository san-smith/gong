// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package parser implements a parser for Go source files. Input may be
// provided in a variety of forms (see the various Parse* functions); the
// output is an abstract syntax tree (AST) representing the Go source. The
// parser is invoked through one of the Parse* functions.
//
// The parser accepts a larger language than is syntactically permitted by
// the Go spec, for simplicity, and for improved robustness in the presence
// of syntax errors. For instance, in method declarations, the receiver is
// treated like an ordinary parameter list and thus may contain multiple
// entries where the spec permits exactly one. Consequently, the corresponding
// field in the AST (ast.FuncDecl.Recv) field is not restricted to one entry.
//
package parser

import (
	"fmt"
	"gong/ast"
	"gong/internal/typeparams"
	"gong/scanner"
	"gong/token"
	"strconv"
	"strings"
	"unicode"
)

// The parser structure holds the parser's internal state.
type parser struct {
	file    *token.File
	errors  scanner.ErrorList
	scanner scanner.Scanner

	// Tracing/debugging
	mode   Mode // parsing mode
	trace  bool // == (mode&Trace != 0)
	indent int  // indentation used for tracing output

	// Comments
	comments    []*ast.CommentGroup
	leadComment *ast.CommentGroup // last lead comment
	lineComment *ast.CommentGroup // last line comment

	// Next token
	pos token.Pos   // token position
	tok token.Token // one token look-ahead
	lit string      // token literal

	// Error recovery
	// (used to limit the number of calls to parser.advance
	// w/o making scanning progress - avoids potential endless
	// loops across multiple parser functions during error recovery)
	syncPos token.Pos // last synchronization position
	syncCnt int       // number of parser.advance calls without progress

	// Non-syntactic parser control
	exprLev int  // < 0: in control clause, >= 0: in expression
	inRhs   bool // if set, the parser is parsing a rhs expression

	imports []*ast.ImportSpec // list of imports
}

func (p *parser) init(fset *token.FileSet, filename string, src []byte, mode Mode) {
	p.file = fset.AddFile(filename, -1, len(src))
	var m scanner.Mode
	if mode&ParseComments != 0 {
		m = scanner.ScanComments
	}
	eh := func(pos token.Position, msg string) { p.errors.Add(pos, msg) }
	p.scanner.Init(p.file, src, eh, m)

	p.mode = mode
	p.trace = mode&Trace != 0 // for convenience (p.trace is used frequently)
	p.next()
}

func (p *parser) parseTypeParams() bool {
	return typeparams.Enabled && p.mode&typeparams.DisallowParsing == 0
}

// ----------------------------------------------------------------------------
// Parsing support

func (p *parser) printTrace(a ...interface{}) {
	const dots = ". . . . . . . . . . . . . . . . . . . . . . . . . . . . . . . . "
	const n = len(dots)
	pos := p.file.Position(p.pos)
	fmt.Printf("%5d:%3d: ", pos.Line, pos.Column)
	i := 2 * p.indent
	for i > n {
		fmt.Print(dots)
		i -= n
	}
	// i <= n
	fmt.Print(dots[0:i])
	fmt.Println(a...)
}

func trace(p *parser, msg string) *parser {
	p.printTrace(msg, "(")
	p.indent++
	return p
}

// Usage pattern: defer un(trace(p, "..."))
func un(p *parser) {
	p.indent--
	p.printTrace(")")
}

// Advance to the next token.
func (p *parser) next0() {
	// Because of one-token look-ahead, print the previous token
	// when tracing as it provides a more readable output. The
	// very first token (!p.pos.IsValid()) is not initialized
	// (it is token.ILLEGAL), so don't print it.
	if p.trace && p.pos.IsValid() {
		s := p.tok.String()
		switch {
		case p.tok.IsLiteral():
			p.printTrace(s, p.lit)
		case p.tok.IsOperator(), p.tok.IsKeyword():
			p.printTrace("\"" + s + "\"")
		default:
			p.printTrace(s)
		}
	}

	p.pos, p.tok, p.lit = p.scanner.Scan()
}

// Consume a comment and return it and the line on which it ends.
func (p *parser) consumeComment() (comment *ast.Comment, endline int) {
	// /*-style comments may end on a different line than where they start.
	// Scan the comment for '\n' chars and adjust endline accordingly.
	endline = p.file.Line(p.pos)
	if p.lit[1] == '*' {
		// don't use range here - no need to decode Unicode code points
		for i := 0; i < len(p.lit); i++ {
			if p.lit[i] == '\n' {
				endline++
			}
		}
	}

	comment = &ast.Comment{Slash: p.pos, Text: p.lit}
	p.next0()

	return
}

// Consume a group of adjacent comments, add it to the parser's
// comments list, and return it together with the line at which
// the last comment in the group ends. A non-comment token or n
// empty lines terminate a comment group.
//
func (p *parser) consumeCommentGroup(n int) (comments *ast.CommentGroup, endline int) {
	var list []*ast.Comment
	endline = p.file.Line(p.pos)
	for p.tok == token.COMMENT && p.file.Line(p.pos) <= endline+n {
		var comment *ast.Comment
		comment, endline = p.consumeComment()
		list = append(list, comment)
	}

	// add comment group to the comments list
	comments = &ast.CommentGroup{List: list}
	p.comments = append(p.comments, comments)

	return
}

// Advance to the next non-comment token. In the process, collect
// any comment groups encountered, and remember the last lead and
// line comments.
//
// A lead comment is a comment group that starts and ends in a
// line without any other tokens and that is followed by a non-comment
// token on the line immediately after the comment group.
//
// A line comment is a comment group that follows a non-comment
// token on the same line, and that has no tokens after it on the line
// where it ends.
//
// Lead and line comments may be considered documentation that is
// stored in the AST.
//
func (p *parser) next() {
	p.leadComment = nil
	p.lineComment = nil
	prev := p.pos
	p.next0()

	if p.tok == token.COMMENT {
		var comment *ast.CommentGroup
		var endline int

		if p.file.Line(p.pos) == p.file.Line(prev) {
			// The comment is on same line as the previous token; it
			// cannot be a lead comment but may be a line comment.
			comment, endline = p.consumeCommentGroup(0)
			if p.file.Line(p.pos) != endline || p.tok == token.EOF {
				// The next token is on a different line, thus
				// the last comment group is a line comment.
				p.lineComment = comment
			}
		}

		// consume successor comments, if any
		endline = -1
		for p.tok == token.COMMENT {
			comment, endline = p.consumeCommentGroup(1)
		}

		if endline+1 == p.file.Line(p.pos) {
			// The next token is following on the line immediately after the
			// comment group, thus the last comment group is a lead comment.
			p.leadComment = comment
		}
	}
}

// A bailout panic is raised to indicate early termination.
type bailout struct{}

func (p *parser) error(pos token.Pos, msg string) {
	if p.trace {
		defer un(trace(p, "error: "+msg))
	}

	epos := p.file.Position(pos)

	// If AllErrors is not set, discard errors reported on the same line
	// as the last recorded error and stop parsing if there are more than
	// 10 errors.
	if p.mode&AllErrors == 0 {
		n := len(p.errors)
		if n > 0 && p.errors[n-1].Pos.Line == epos.Line {
			return // discard - likely a spurious error
		}
		if n > 10 {
			panic(bailout{})
		}
	}

	p.errors.Add(epos, msg)
}

func (p *parser) errorExpected(pos token.Pos, msg string) {
	msg = "expected " + msg
	if pos == p.pos {
		// the error happened at the current position;
		// make the error message more specific
		switch {
		case p.tok == token.SEMICOLON && p.lit == "\n":
			msg += ", found newline"
		case p.tok.IsLiteral():
			// print 123 rather than 'INT', etc.
			msg += ", found " + p.lit
		default:
			msg += ", found '" + p.tok.String() + "'"
		}
	}
	p.error(pos, msg)
}

func (p *parser) expect(tok token.Token) token.Pos {
	pos := p.pos
	if p.tok != tok {
		p.errorExpected(pos, "'"+tok.String()+"'")
	}
	p.next() // make progress
	return pos
}

// expect2 is like expect, but it returns an invalid position
// if the expected token is not found.
func (p *parser) expect2(tok token.Token) (pos token.Pos) {
	if p.tok == tok {
		pos = p.pos
	} else {
		p.errorExpected(p.pos, "'"+tok.String()+"'")
	}
	p.next() // make progress
	return
}

// expectClosing is like expect but provides a better error message
// for the common case of a missing comma before a newline.
//
func (p *parser) expectClosing(tok token.Token, context string) token.Pos {
	if p.tok != tok && p.tok == token.SEMICOLON && p.lit == "\n" {
		p.error(p.pos, "missing ',' before newline in "+context)
		p.next()
	}
	return p.expect(tok)
}

func (p *parser) expectSemi() {
	// semicolon is optional before a closing ')' or '}'
	if p.tok != token.RPAREN && p.tok != token.RBRACE {
		switch p.tok {
		case token.COMMA:
			// permit a ',' instead of a ';' but complain
			p.errorExpected(p.pos, "';'")
			fallthrough
		case token.SEMICOLON:
			p.next()
		default:
			p.errorExpected(p.pos, "';'")
			p.advance(stmtStart)
		}
	}
}

func (p *parser) atComma(context string, follow token.Token) bool {
	if p.tok == token.COMMA {
		return true
	}
	if p.tok != follow {
		msg := "missing ','"
		if p.tok == token.SEMICOLON && p.lit == "\n" {
			msg += " before newline"
		}
		p.error(p.pos, msg+" in "+context)
		return true // "insert" comma and continue
	}
	return false
}

func assert(cond bool, msg string) {
	if !cond {
		panic("go/parser internal error: " + msg)
	}
}

// advance consumes tokens until the current token p.tok
// is in the 'to' set, or token.EOF. For error recovery.
func (p *parser) advance(to map[token.Token]bool) {
	for ; p.tok != token.EOF; p.next() {
		if to[p.tok] {
			// Return only if parser made some progress since last
			// sync or if it has not reached 10 advance calls without
			// progress. Otherwise consume at least one token to
			// avoid an endless parser loop (it is possible that
			// both parseOperand and parseStmt call advance and
			// correctly do not advance, thus the need for the
			// invocation limit p.syncCnt).
			if p.pos == p.syncPos && p.syncCnt < 10 {
				p.syncCnt++
				return
			}
			if p.pos > p.syncPos {
				p.syncPos = p.pos
				p.syncCnt = 0
				return
			}
			// Reaching here indicates a parser bug, likely an
			// incorrect token list in this function, but it only
			// leads to skipping of possibly correct code if a
			// previous error is present, and thus is preferred
			// over a non-terminating parse.
		}
	}
}

var stmtStart = map[token.Token]bool{
	token.CONST:  true,
	token.IF:     true,
	token.RETURN: true,
	token.TYPE:   true,
	token.VAR:    true,
}

var declStart = map[token.Token]bool{
	token.CONST: true,
	token.TYPE:  true,
	token.VAR:   true,
}

var exprEnd = map[token.Token]bool{
	token.COMMA:     true,
	token.COLON:     true,
	token.SEMICOLON: true,
	token.RPAREN:    true,
	token.RBRACK:    true,
	token.RBRACE:    true,
}

// safePos returns a valid file position for a given position: If pos
// is valid to begin with, safePos returns pos. If pos is out-of-range,
// safePos returns the EOF position.
//
// This is hack to work around "artificial" end positions in the AST which
// are computed by adding 1 to (presumably valid) token positions. If the
// token positions are invalid due to parse errors, the resulting end position
// may be past the file's EOF position, which would lead to panics if used
// later on.
//
func (p *parser) safePos(pos token.Pos) (res token.Pos) {
	defer func() {
		if recover() != nil {
			res = token.Pos(p.file.Base() + p.file.Size()) // EOF position
		}
	}()
	_ = p.file.Offset(pos) // trigger a panic if position is out-of-range
	return pos
}

// ----------------------------------------------------------------------------
// Identifiers

func (p *parser) parseIdent() *ast.Ident {
	pos := p.pos
	name := "_"
	if p.tok == token.IDENT {
		name = p.lit
		p.next()
	} else {
		p.expect(token.IDENT) // use expect() error handling
	}
	return &ast.Ident{NamePos: pos, Name: name}
}

func (p *parser) parseIdentList() (list []*ast.Ident) {
	if p.trace {
		defer un(trace(p, "IdentList"))
	}

	list = append(list, p.parseIdent())
	for p.tok == token.COMMA {
		p.next()
		list = append(list, p.parseIdent())
	}

	return
}

// ----------------------------------------------------------------------------
// Common productions

// If lhs is set, result list elements which are identifiers are not resolved.
func (p *parser) parseExprList() (list []ast.Expr) {
	if p.trace {
		defer un(trace(p, "ExpressionList"))
	}

	list = append(list, p.checkExpr(p.parseExpr()))
	for p.tok == token.COMMA {
		p.next()
		list = append(list, p.checkExpr(p.parseExpr()))
	}

	return
}

func (p *parser) parseList(inRhs bool) []ast.Expr {
	old := p.inRhs
	p.inRhs = inRhs
	list := p.parseExprList()
	p.inRhs = old
	return list
}

// ----------------------------------------------------------------------------
// Types

func (p *parser) parseType() ast.Expr {
	if p.trace {
		defer un(trace(p, "Type"))
	}

	typ := p.tryIdentOrType()

	if typ == nil {
		pos := p.pos
		p.errorExpected(pos, "type")
		p.advance(exprEnd)
		return &ast.BadExpr{From: pos, To: p.pos}
	}

	return typ
}

func (p *parser) parseQualifiedIdent(ident *ast.Ident) ast.Expr {
	if p.trace {
		defer un(trace(p, "QualifiedIdent"))
	}

	typ := p.parseTypeName(ident)
	if p.tok == token.LBRACK && p.parseTypeParams() {
		typ = p.parseTypeInstance(typ)
	}

	return typ
}

// If the result is an identifier, it is not resolved.
func (p *parser) parseTypeName(ident *ast.Ident) ast.Expr {
	if p.trace {
		defer un(trace(p, "TypeName"))
	}

	if ident == nil {
		ident = p.parseIdent()
	}

	if p.tok == token.PERIOD {
		// ident is a package name
		p.next()
		sel := p.parseIdent()
		return &ast.SelectorExpr{X: ident, Sel: sel}
	}

	return ident
}

func (p *parser) parseArrayLen() ast.Expr {
	if p.trace {
		defer un(trace(p, "ArrayLen"))
	}

	p.exprLev++
	var len ast.Expr
	// always permit ellipsis for more fault-tolerant parsing
	if p.tok == token.ELLIPSIS {
		len = &ast.Ellipsis{Ellipsis: p.pos}
		p.next()
	} else if p.tok != token.RBRACK {
		len = p.parseRhs()
	}
	p.exprLev--

	return len
}

func (p *parser) parseArrayFieldOrTypeInstance(x *ast.Ident) (*ast.Ident, ast.Expr) {
	if p.trace {
		defer un(trace(p, "ArrayFieldOrTypeInstance"))
	}

	// TODO(gri) Should we allow a trailing comma in a type argument
	//           list such as T[P,]? (We do in parseTypeInstance).
	lbrack := p.expect(token.LBRACK)
	var args []ast.Expr
	var firstComma token.Pos
	// TODO(rfindley): consider changing parseRhsOrType so that this function variable
	// is not needed.
	argparser := p.parseRhsOrType
	if !p.parseTypeParams() {
		argparser = p.parseRhs
	}
	if p.tok != token.RBRACK {
		p.exprLev++
		args = append(args, argparser())
		for p.tok == token.COMMA {
			if !firstComma.IsValid() {
				firstComma = p.pos
			}
			p.next()
			args = append(args, argparser())
		}
		p.exprLev--
	}
	rbrack := p.expect(token.RBRACK)

	// if len(args) == 0 {
	// 	// x []E
	// 	elt := p.parseType()
	// 	return x, &ast.ArrayType{Lbrack: lbrack, Elt: elt}
	// }

	// x [P]E or x[P]
	if len(args) == 1 {
		// elt := p.tryIdentOrType()
		// if elt != nil {
		// 	// x [P]E
		// 	return x, &ast.ArrayType{Lbrack: lbrack, Len: args[0], Elt: elt}
		// }
		if !p.parseTypeParams() {
			p.error(rbrack, "missing element type in array type expression")
			return nil, &ast.BadExpr{From: args[0].Pos(), To: args[0].End()}
		}
	}

	if !p.parseTypeParams() {
		p.error(firstComma, "expected ']', found ','")
		return x, &ast.BadExpr{From: args[0].Pos(), To: args[len(args)-1].End()}
	}

	// x[P], x[P1, P2], ...
	return nil, &ast.IndexExpr{X: x, Lbrack: lbrack, Index: typeparams.PackExpr(args), Rbrack: rbrack}
}

func (p *parser) parseFieldDecl() *ast.Field {
	if p.trace {
		defer un(trace(p, "FieldDecl"))
	}

	doc := p.leadComment

	var names []*ast.Ident
	var typ ast.Expr
	if p.tok == token.IDENT {
		name := p.parseIdent()
		if p.tok == token.PERIOD || p.tok == token.STRING || p.tok == token.SEMICOLON || p.tok == token.RBRACE {
			// embedded type
			typ = name
			if p.tok == token.PERIOD {
				typ = p.parseQualifiedIdent(name)
			}
		} else {
			// name1, name2, ... T
			names = []*ast.Ident{name}
			for p.tok == token.COMMA {
				p.next()
				names = append(names, p.parseIdent())
			}
			// Careful dance: We don't know if we have an embedded instantiated
			// type T[P1, P2, ...] or a field T of array type []E or [P]E.
			if len(names) == 1 && p.tok == token.LBRACK {
				name, typ = p.parseArrayFieldOrTypeInstance(name)
				if name == nil {
					names = nil
				}
			} else {
				// T P
				typ = p.parseType()
			}
		}
	} else {
		// embedded, possibly generic type
		// (using the enclosing parentheses to distinguish it from a named field declaration)
		// TODO(rFindley) confirm that this doesn't allow parenthesized embedded type
		typ = p.parseType()
	}

	var tag *ast.BasicLit
	if p.tok == token.STRING {
		tag = &ast.BasicLit{ValuePos: p.pos, Kind: p.tok, Value: p.lit}
		p.next()
	}

	p.expectSemi() // call before accessing p.linecomment

	field := &ast.Field{Doc: doc, Names: names, Type: typ, Tag: tag, Comment: p.lineComment}
	return field
}

func (p *parser) parsePointerType() *ast.StarExpr {
	if p.trace {
		defer un(trace(p, "PointerType"))
	}

	star := p.expect(token.MUL)
	base := p.parseType()

	return &ast.StarExpr{Star: star, X: base}
}

func (p *parser) parseDotsType() *ast.Ellipsis {
	if p.trace {
		defer un(trace(p, "DotsType"))
	}

	pos := p.expect(token.ELLIPSIS)
	elt := p.parseType()

	return &ast.Ellipsis{Ellipsis: pos, Elt: elt}
}

type field struct {
	name *ast.Ident
	typ  ast.Expr
}

func (p *parser) parseParamDecl(name *ast.Ident) (f field) {
	// TODO(rFindley) compare with parser.paramDeclOrNil in the syntax package
	if p.trace {
		defer un(trace(p, "ParamDeclOrNil"))
	}

	ptok := p.tok
	if name != nil {
		p.tok = token.IDENT // force token.IDENT case in switch below
	}

	switch p.tok {
	case token.IDENT:
		if name != nil {
			f.name = name
			p.tok = ptok
		} else {
			f.name = p.parseIdent()
		}
		switch p.tok {
		case token.IDENT, token.MUL, token.FUN, token.LPAREN:
			// name type
			f.typ = p.parseType()

		case token.LBRACK:
			// name[type1, type2, ...] or name []type or name [len]type
			f.name, f.typ = p.parseArrayFieldOrTypeInstance(f.name)

		case token.ELLIPSIS:
			// name ...type
			f.typ = p.parseDotsType()

		case token.PERIOD:
			// qualified.typename
			f.typ = p.parseQualifiedIdent(f.name)
			f.name = nil
		}

	case token.MUL, token.FUN, token.LBRACK, token.LPAREN:
		// type
		f.typ = p.parseType()

	case token.ELLIPSIS:
		// ...type
		// (always accepted)
		f.typ = p.parseDotsType()

	default:
		p.errorExpected(p.pos, ")")
		p.advance(exprEnd)
	}

	return
}

func (p *parser) parseParameterList(name0 *ast.Ident, closing token.Token, parseParamDecl func(*ast.Ident) field, tparams bool) (params []*ast.Field) {
	if p.trace {
		defer un(trace(p, "ParameterList"))
	}

	pos := p.pos
	if name0 != nil {
		pos = name0.Pos()
	}

	var list []field
	var named int // number of parameters that have an explicit name and type

	for name0 != nil || p.tok != closing && p.tok != token.EOF {
		par := parseParamDecl(name0)
		name0 = nil // 1st name was consumed if present
		if par.name != nil || par.typ != nil {
			list = append(list, par)
			if par.name != nil && par.typ != nil {
				named++
			}
		}
		if !p.atComma("parameter list", closing) {
			break
		}
		p.next()
	}

	if len(list) == 0 {
		return // not uncommon
	}

	// TODO(gri) parameter distribution and conversion to []*ast.Field
	//           can be combined and made more efficient

	// distribute parameter types
	if named == 0 {
		// all unnamed => found names are type names
		for i := 0; i < len(list); i++ {
			par := &list[i]
			if typ := par.name; typ != nil {
				par.typ = typ
				par.name = nil
			}
		}
		if tparams {
			p.error(pos, "all type parameters must be named")
		}
	} else if named != len(list) {
		// some named => all must be named
		ok := true
		var typ ast.Expr
		for i := len(list) - 1; i >= 0; i-- {
			if par := &list[i]; par.typ != nil {
				typ = par.typ
				if par.name == nil {
					ok = false
					n := ast.NewIdent("_")
					n.NamePos = typ.Pos() // correct position
					par.name = n
				}
			} else if typ != nil {
				par.typ = typ
			} else {
				// par.typ == nil && typ == nil => we only have a par.name
				ok = false
				par.typ = &ast.BadExpr{From: par.name.Pos(), To: p.pos}
			}
		}
		if !ok {
			if tparams {
				p.error(pos, "all type parameters must be named")
			} else {
				p.error(pos, "mixed named and unnamed parameters")
			}
		}
	}

	// convert list []*ast.Field
	if named == 0 {
		// parameter list consists of types only
		for _, par := range list {
			assert(par.typ != nil, "nil type in unnamed parameter list")
			params = append(params, &ast.Field{Type: par.typ})
		}
		return
	}

	// parameter list consists of named parameters with types
	var names []*ast.Ident
	var typ ast.Expr
	addParams := func() {
		assert(typ != nil, "nil type in named parameter list")
		field := &ast.Field{Names: names, Type: typ}
		params = append(params, field)
		names = nil
	}
	for _, par := range list {
		if par.typ != typ {
			if len(names) > 0 {
				addParams()
			}
			typ = par.typ
		}
		names = append(names, par.name)
	}
	if len(names) > 0 {
		addParams()
	}
	return
}

func (p *parser) parseParameters(acceptTParams bool) (tparams, params *ast.FieldList) {
	if p.trace {
		defer un(trace(p, "Parameters"))
	}

	if p.parseTypeParams() && acceptTParams && p.tok == token.LBRACK {
		opening := p.pos
		p.next()
		// [T any](params) syntax
		list := p.parseParameterList(nil, token.RBRACK, p.parseParamDecl, true)
		rbrack := p.expect(token.RBRACK)
		tparams = &ast.FieldList{Opening: opening, List: list, Closing: rbrack}
		// Type parameter lists must not be empty.
		if tparams.NumFields() == 0 {
			p.error(tparams.Closing, "empty type parameter list")
			tparams = nil // avoid follow-on errors
		}
	}

	opening := p.expect(token.LPAREN)

	var fields []*ast.Field
	if p.tok != token.RPAREN {
		fields = p.parseParameterList(nil, token.RPAREN, p.parseParamDecl, false)
	}

	rparen := p.expect(token.RPAREN)
	params = &ast.FieldList{Opening: opening, List: fields, Closing: rparen}

	return
}

func (p *parser) parseResult() *ast.FieldList {
	if p.trace {
		defer un(trace(p, "Result"))
	}

	if p.tok == token.LPAREN {
		_, results := p.parseParameters(false)
		return results
	}

	typ := p.tryIdentOrType()
	if typ != nil {
		list := make([]*ast.Field, 1)
		list[0] = &ast.Field{Type: typ}
		return &ast.FieldList{List: list}
	}

	return nil
}

func (p *parser) parseFuncType() *ast.FunType {
	if p.trace {
		defer un(trace(p, "FunType"))
	}

	pos := p.expect(token.FUN)
	tparams, params := p.parseParameters(true)
	if tparams != nil {
		p.error(tparams.Pos(), "function type cannot have type parameters")
	}
	results := p.parseResult()

	return &ast.FunType{Fun: pos, Params: params, Results: results}
}

func (p *parser) parseMethodSpec() *ast.Field {
	if p.trace {
		defer un(trace(p, "MethodSpec"))
	}

	doc := p.leadComment
	var idents []*ast.Ident
	var typ ast.Expr
	x := p.parseTypeName(nil)
	if ident, _ := x.(*ast.Ident); ident != nil {
		switch {
		case p.tok == token.LBRACK && p.parseTypeParams():
			// generic method or embedded instantiated type
			lbrack := p.pos
			p.next()
			p.exprLev++
			x := p.parseExpr()
			p.exprLev--
			if name0, _ := x.(*ast.Ident); name0 != nil && p.tok != token.COMMA && p.tok != token.RBRACK {
				// generic method m[T any]
				list := p.parseParameterList(name0, token.RBRACK, p.parseParamDecl, true)
				rbrack := p.expect(token.RBRACK)
				tparams := &ast.FieldList{Opening: lbrack, List: list, Closing: rbrack}
				// TODO(rfindley) refactor to share code with parseFuncType.
				_, params := p.parseParameters(false)
				results := p.parseResult()
				idents = []*ast.Ident{ident}
				typ = &ast.FunType{Fun: token.NoPos, Params: params, Results: results}
				typeparams.Set(typ, tparams)
			} else {
				// embedded instantiated type
				// TODO(rfindley) should resolve all identifiers in x.
				list := []ast.Expr{x}
				if p.atComma("type argument list", token.RBRACK) {
					p.exprLev++
					for p.tok != token.RBRACK && p.tok != token.EOF {
						list = append(list, p.parseType())
						if !p.atComma("type argument list", token.RBRACK) {
							break
						}
						p.next()
					}
					p.exprLev--
				}
				rbrack := p.expectClosing(token.RBRACK, "type argument list")
				typ = &ast.IndexExpr{X: ident, Lbrack: lbrack, Index: typeparams.PackExpr(list), Rbrack: rbrack}
			}
		case p.tok == token.LPAREN:
			// ordinary method
			// TODO(rfindley) refactor to share code with parseFuncType.
			_, params := p.parseParameters(false)
			results := p.parseResult()
			idents = []*ast.Ident{ident}
			typ = &ast.FunType{Fun: token.NoPos, Params: params, Results: results}
		default:
			// embedded type
			typ = x
		}
	} else {
		// embedded, possibly instantiated type
		typ = x
		if p.tok == token.LBRACK && p.parseTypeParams() {
			// embedded instantiated interface
			typ = p.parseTypeInstance(typ)
		}
	}
	p.expectSemi() // call before accessing p.linecomment

	spec := &ast.Field{Doc: doc, Names: idents, Type: typ, Comment: p.lineComment}

	return spec
}

func (p *parser) parseTypeInstance(typ ast.Expr) ast.Expr {
	assert(p.parseTypeParams(), "parseTypeInstance while not parsing type params")
	if p.trace {
		defer un(trace(p, "TypeInstance"))
	}

	opening := p.expect(token.LBRACK)

	p.exprLev++
	var list []ast.Expr
	for p.tok != token.RBRACK && p.tok != token.EOF {
		list = append(list, p.parseType())
		if !p.atComma("type argument list", token.RBRACK) {
			break
		}
		p.next()
	}
	p.exprLev--

	closing := p.expectClosing(token.RBRACK, "type argument list")

	return &ast.IndexExpr{X: typ, Lbrack: opening, Index: typeparams.PackExpr(list), Rbrack: closing}
}

func (p *parser) tryIdentOrType() ast.Expr {
	switch p.tok {
	case token.IDENT:
		typ := p.parseTypeName(nil)
		if p.tok == token.LBRACK && p.parseTypeParams() {
			typ = p.parseTypeInstance(typ)
		}
		return typ
	case token.MUL:
		return p.parsePointerType()
	case token.FUN:
		typ := p.parseFuncType()
		return typ
	case token.LPAREN:
		lparen := p.pos
		p.next()
		typ := p.parseType()
		rparen := p.expect(token.RPAREN)
		return &ast.ParenExpr{Lparen: lparen, X: typ, Rparen: rparen}
	}

	// no type found
	return nil
}

// ----------------------------------------------------------------------------
// Blocks

func (p *parser) parseStmtList() (list []ast.Stmt) {
	if p.trace {
		defer un(trace(p, "StatementList"))
	}

	for p.tok != token.RBRACE && p.tok != token.EOF {
		list = append(list, p.parseStmt())
	}

	return
}

func (p *parser) parseBody() *ast.BlockStmt {
	if p.trace {
		defer un(trace(p, "Body"))
	}

	lbrace := p.expect(token.LBRACE)
	list := p.parseStmtList()
	rbrace := p.expect2(token.RBRACE)

	return &ast.BlockStmt{Lbrace: lbrace, List: list, Rbrace: rbrace}
}

func (p *parser) parseBlockStmt() *ast.BlockStmt {
	if p.trace {
		defer un(trace(p, "BlockStmt"))
	}

	lbrace := p.expect(token.LBRACE)
	list := p.parseStmtList()
	rbrace := p.expect2(token.RBRACE)

	return &ast.BlockStmt{Lbrace: lbrace, List: list, Rbrace: rbrace}
}

// ----------------------------------------------------------------------------
// Expressions

func (p *parser) parseFuncTypeOrLit() ast.Expr {
	if p.trace {
		defer un(trace(p, "FuncTypeOrLit"))
	}

	typ := p.parseFuncType()
	if p.tok != token.LBRACE {
		// function type only
		return typ
	}

	p.exprLev++
	body := p.parseBody()
	p.exprLev--

	return &ast.FunLit{Type: typ, Body: body}
}

// parseOperand may return an expression or a raw type (incl. array
// types of the form [...]T. Callers must verify the result.
//
func (p *parser) parseOperand() ast.Expr {
	if p.trace {
		defer un(trace(p, "Operand"))
	}

	switch p.tok {
	case token.IDENT:
		x := p.parseIdent()
		return x

	case token.INT, token.FLOAT, token.IMAG, token.CHAR, token.STRING:
		x := &ast.BasicLit{ValuePos: p.pos, Kind: p.tok, Value: p.lit}
		p.next()
		return x

	case token.LPAREN:
		lparen := p.pos
		p.next()
		p.exprLev++
		x := p.parseRhsOrType() // types may be parenthesized: (some type)
		p.exprLev--
		rparen := p.expect(token.RPAREN)
		return &ast.ParenExpr{Lparen: lparen, X: x, Rparen: rparen}

	case token.FUN:
		return p.parseFuncTypeOrLit()
	}

	if typ := p.tryIdentOrType(); typ != nil { // do not consume trailing type parameters
		// could be type for composite literal or conversion
		_, isIdent := typ.(*ast.Ident)
		assert(!isIdent, "type cannot be identifier")
		return typ
	}

	// we have an error
	pos := p.pos
	p.errorExpected(pos, "operand")
	p.advance(stmtStart)
	return &ast.BadExpr{From: pos, To: p.pos}
}

func (p *parser) parseSelector(x ast.Expr) ast.Expr {
	if p.trace {
		defer un(trace(p, "Selector"))
	}

	sel := p.parseIdent()

	return &ast.SelectorExpr{X: x, Sel: sel}
}

func (p *parser) parseIndexOrSliceOrInstance(x ast.Expr) ast.Expr {
	if p.trace {
		defer un(trace(p, "parseIndexOrSliceOrInstance"))
	}

	lbrack := p.expect(token.LBRACK)
	if p.tok == token.RBRACK {
		// empty index, slice or index expressions are not permitted;
		// accept them for parsing tolerance, but complain
		p.errorExpected(p.pos, "operand")
		rbrack := p.pos
		p.next()
		return &ast.IndexExpr{
			X:      x,
			Lbrack: lbrack,
			Index:  &ast.BadExpr{From: rbrack, To: rbrack},
			Rbrack: rbrack,
		}
	}
	p.exprLev++

	const N = 3 // change the 3 to 2 to disable 3-index slices
	var args []ast.Expr
	var index [N]ast.Expr
	var firstComma token.Pos
	if p.tok != token.COLON {
		// We can't know if we have an index expression or a type instantiation;
		// so even if we see a (named) type we are not going to be in type context.
		index[0] = p.parseRhsOrType()
	}

	switch p.tok {
	case token.COMMA:
		firstComma = p.pos
		// instance expression
		args = append(args, index[0])
		for p.tok == token.COMMA {
			p.next()
			if p.tok != token.RBRACK && p.tok != token.EOF {
				args = append(args, p.parseType())
			}
		}
	}

	p.exprLev--
	rbrack := p.expect(token.RBRACK)

	if len(args) == 0 {
		// index expression
		return &ast.IndexExpr{X: x, Lbrack: lbrack, Index: index[0], Rbrack: rbrack}
	}

	if !p.parseTypeParams() {
		p.error(firstComma, "expected ']', found ','")
		return &ast.BadExpr{From: args[0].Pos(), To: args[len(args)-1].End()}
	}

	// instance expression
	return &ast.IndexExpr{X: x, Lbrack: lbrack, Index: typeparams.PackExpr(args), Rbrack: rbrack}
}

func (p *parser) parseCallOrConversion(fun ast.Expr) *ast.CallExpr {
	if p.trace {
		defer un(trace(p, "CallOrConversion"))
	}

	lparen := p.expect(token.LPAREN)
	p.exprLev++
	var list []ast.Expr
	var ellipsis token.Pos
	for p.tok != token.RPAREN && p.tok != token.EOF && !ellipsis.IsValid() {
		list = append(list, p.parseRhsOrType()) // builtins may expect a type: make(some type, ...)
		if p.tok == token.ELLIPSIS {
			ellipsis = p.pos
			p.next()
		}
		if !p.atComma("argument list", token.RPAREN) {
			break
		}
		p.next()
	}
	p.exprLev--
	rparen := p.expectClosing(token.RPAREN, "argument list")

	return &ast.CallExpr{Fun: fun, Lparen: lparen, Args: list, Ellipsis: ellipsis, Rparen: rparen}
}

func (p *parser) parseValue() ast.Expr {
	if p.trace {
		defer un(trace(p, "Element"))
	}

	x := p.checkExpr(p.parseExpr())

	return x
}

func (p *parser) parseElement() ast.Expr {
	if p.trace {
		defer un(trace(p, "Element"))
	}

	x := p.parseValue()
	if p.tok == token.COLON {
		colon := p.pos
		p.next()
		x = &ast.KeyValueExpr{Key: x, Colon: colon, Value: p.parseValue()}
	}

	return x
}

func (p *parser) parseElementList() (list []ast.Expr) {
	if p.trace {
		defer un(trace(p, "ElementList"))
	}

	for p.tok != token.RBRACE && p.tok != token.EOF {
		list = append(list, p.parseElement())
		if !p.atComma("composite literal", token.RBRACE) {
			break
		}
		p.next()
	}

	return
}

// checkExpr checks that x is an expression (and not a type).
func (p *parser) checkExpr(x ast.Expr) ast.Expr {
	switch unparen(x).(type) {
	case *ast.BadExpr:
	case *ast.Ident:
	case *ast.BasicLit:
	case *ast.FunLit:
	case *ast.ParenExpr:
		panic("unreachable")
	case *ast.SelectorExpr:
	case *ast.IndexExpr:
	case *ast.CallExpr:
	case *ast.StarExpr:
	case *ast.UnaryExpr:
	case *ast.BinaryExpr:
	default:
		// all other nodes are not proper expressions
		p.errorExpected(x.Pos(), "expression")
		x = &ast.BadExpr{From: x.Pos(), To: p.safePos(x.End())}
	}
	return x
}

// If x is of the form (T), unparen returns unparen(T), otherwise it returns x.
func unparen(x ast.Expr) ast.Expr {
	if p, isParen := x.(*ast.ParenExpr); isParen {
		x = unparen(p.X)
	}
	return x
}

// checkExprOrType checks that x is an expression or a type
// (and not a raw type such as [...]T).
//
func (p *parser) checkExprOrType(x ast.Expr) ast.Expr {
	switch unparen(x).(type) {
	case *ast.ParenExpr:
		panic("unreachable")

	}

	// all other nodes are expressions or types
	return x
}

func (p *parser) parsePrimaryExpr() (x ast.Expr) {
	if p.trace {
		defer un(trace(p, "PrimaryExpr"))
	}

	x = p.parseOperand()
	for {
		switch p.tok {
		case token.PERIOD:
			p.next()
			switch p.tok {
			case token.IDENT:
				x = p.parseSelector(p.checkExprOrType(x))
			default:
				pos := p.pos
				p.errorExpected(pos, "selector or type assertion")
				// TODO(rFindley) The check for token.RBRACE below is a targeted fix
				//                to error recovery sufficient to make the x/tools tests to
				//                pass with the new parsing logic introduced for type
				//                parameters. Remove this once error recovery has been
				//                more generally reconsidered.
				if p.tok != token.RBRACE {
					p.next() // make progress
				}
				sel := &ast.Ident{NamePos: pos, Name: "_"}
				x = &ast.SelectorExpr{X: x, Sel: sel}
			}
		case token.LBRACK:
			x = p.parseIndexOrSliceOrInstance(p.checkExpr(x))
		case token.LPAREN:
			x = p.parseCallOrConversion(p.checkExprOrType(x))
		case token.LBRACE:
			// operand may have returned a parenthesized complit
			// type; accept it but complain if we have a complit
			t := unparen(x)
			// determine if '{' belongs to a composite literal or a block statement
			switch t.(type) {
			case *ast.BadExpr, *ast.Ident, *ast.SelectorExpr:
				if p.exprLev < 0 {
					return
				}
				// x is possibly a composite literal type
			case *ast.IndexExpr:
				if p.exprLev < 0 {
					return
				}
				// x is possibly a composite literal type

			default:
				return
			}
		default:
			return
		}
	}
}

func (p *parser) parseUnaryExpr() ast.Expr {
	if p.trace {
		defer un(trace(p, "UnaryExpr"))
	}

	switch p.tok {
	case token.ADD, token.SUB, token.NOT, token.XOR, token.AND:
		pos, op := p.pos, p.tok
		p.next()
		x := p.parseUnaryExpr()
		return &ast.UnaryExpr{OpPos: pos, Op: op, X: p.checkExpr(x)}

	case token.MUL:
		// pointer type or unary "*" expression
		pos := p.pos
		p.next()
		x := p.parseUnaryExpr()
		return &ast.StarExpr{Star: pos, X: p.checkExprOrType(x)}
	}

	return p.parsePrimaryExpr()
}

func (p *parser) tokPrec() (token.Token, int) {
	tok := p.tok
	if p.inRhs && tok == token.ASSIGN {
		tok = token.EQL
	}
	return tok, tok.Precedence()
}

func (p *parser) parseBinaryExpr(prec1 int) ast.Expr {
	if p.trace {
		defer un(trace(p, "BinaryExpr"))
	}

	x := p.parseUnaryExpr()
	for {
		op, oprec := p.tokPrec()
		if oprec < prec1 {
			return x
		}
		pos := p.expect(op)
		y := p.parseBinaryExpr(oprec + 1)
		x = &ast.BinaryExpr{X: p.checkExpr(x), OpPos: pos, Op: op, Y: p.checkExpr(y)}
	}
}

// The result may be a type or even a raw type ([...]int). Callers must
// check the result (using checkExpr or checkExprOrType), depending on
// context.
func (p *parser) parseExpr() ast.Expr {
	if p.trace {
		defer un(trace(p, "Expression"))
	}

	return p.parseBinaryExpr(token.LowestPrec + 1)
}

func (p *parser) parseRhs() ast.Expr {
	old := p.inRhs
	p.inRhs = true
	x := p.checkExpr(p.parseExpr())
	p.inRhs = old
	return x
}

func (p *parser) parseRhsOrType() ast.Expr {
	old := p.inRhs
	p.inRhs = true
	x := p.checkExprOrType(p.parseExpr())
	p.inRhs = old
	return x
}

// ----------------------------------------------------------------------------
// Statements

// Parsing modes for parseSimpleStmt.
const (
	basic = iota
	labelOk
	rangeOk
)

// parseSimpleStmt returns true as 2nd result if it parsed the assignment
// of a range clause (with mode == rangeOk). The returned statement is an
// assignment with a right-hand side that is a single unary expression of
// the form "range x". No guarantees are given for the left-hand side.
func (p *parser) parseSimpleStmt(mode int) (ast.Stmt, bool) {
	if p.trace {
		defer un(trace(p, "SimpleStmt"))
	}

	x := p.parseList(false)

	switch p.tok {
	case
		token.DEFINE, token.ASSIGN, token.ADD_ASSIGN,
		token.SUB_ASSIGN, token.MUL_ASSIGN, token.QUO_ASSIGN,
		token.REM_ASSIGN, token.AND_ASSIGN, token.OR_ASSIGN,
		token.XOR_ASSIGN, token.SHL_ASSIGN, token.SHR_ASSIGN, token.AND_NOT_ASSIGN:
		// assignment statement, possibly part of a range clause
		pos, tok := p.pos, p.tok
		p.next()
		var y []ast.Expr
		isRange := false

		y = p.parseList(true)

		as := &ast.AssignStmt{Lhs: x, TokPos: pos, Tok: tok, Rhs: y}
		if tok == token.DEFINE {
			p.checkAssignStmt(as)
		}
		return as, isRange
	}

	if len(x) > 1 {
		p.errorExpected(x[0].Pos(), "1 expression")
		// continue with first expression
	}

	switch p.tok {

	case token.INC, token.DEC:
		// increment or decrement
		s := &ast.IncDecStmt{X: x[0], TokPos: p.pos, Tok: p.tok}
		p.next()
		return s, false
	}

	// expression
	return &ast.ExprStmt{X: x[0]}, false
}

func (p *parser) checkAssignStmt(as *ast.AssignStmt) {
	for _, x := range as.Lhs {
		if _, isIdent := x.(*ast.Ident); !isIdent {
			p.errorExpected(x.Pos(), "identifier on left side of :=")
		}
	}
}

func (p *parser) parseCallExpr(callType string) *ast.CallExpr {
	x := p.parseRhsOrType() // could be a conversion: (some type)(x)
	if call, isCall := x.(*ast.CallExpr); isCall {
		return call
	}
	if _, isBad := x.(*ast.BadExpr); !isBad {
		// only report error if it's a new one
		p.error(p.safePos(x.End()), fmt.Sprintf("function must be invoked in %s statement", callType))
	}
	return nil
}

func (p *parser) parseReturnStmt() *ast.ReturnStmt {
	if p.trace {
		defer un(trace(p, "ReturnStmt"))
	}

	pos := p.pos
	p.expect(token.RETURN)
	var x []ast.Expr
	if p.tok != token.SEMICOLON && p.tok != token.RBRACE {
		x = p.parseList(true)
	}
	p.expectSemi()

	return &ast.ReturnStmt{Return: pos, Results: x}
}

func (p *parser) makeExpr(s ast.Stmt, want string) ast.Expr {
	if s == nil {
		return nil
	}
	if es, isExpr := s.(*ast.ExprStmt); isExpr {
		return p.checkExpr(es.X)
	}
	found := "simple statement"
	if _, isAss := s.(*ast.AssignStmt); isAss {
		found = "assignment"
	}
	p.error(s.Pos(), fmt.Sprintf("expected %s, found %s (missing parentheses around composite literal?)", want, found))
	return &ast.BadExpr{From: s.Pos(), To: p.safePos(s.End())}
}

// parseIfHeader is an adjusted version of parser.header
// in cmd/compile/internal/syntax/parser.go, which has
// been tuned for better error handling.
func (p *parser) parseIfHeader() (init ast.Stmt, cond ast.Expr) {
	if p.tok == token.LBRACE {
		p.error(p.pos, "missing condition in if statement")
		cond = &ast.BadExpr{From: p.pos, To: p.pos}
		return
	}
	// p.tok != token.LBRACE

	prevLev := p.exprLev
	p.exprLev = -1

	if p.tok != token.SEMICOLON {
		// accept potential variable declaration but complain
		if p.tok == token.VAR {
			p.next()
			p.error(p.pos, "var declaration not allowed in 'IF' initializer")
		}
		init, _ = p.parseSimpleStmt(basic)
	}

	var condStmt ast.Stmt
	var semi struct {
		pos token.Pos
		lit string // ";" or "\n"; valid if pos.IsValid()
	}
	if p.tok != token.LBRACE {
		if p.tok == token.SEMICOLON {
			semi.pos = p.pos
			semi.lit = p.lit
			p.next()
		} else {
			p.expect(token.SEMICOLON)
		}
		if p.tok != token.LBRACE {
			condStmt, _ = p.parseSimpleStmt(basic)
		}
	} else {
		condStmt = init
		init = nil
	}

	if condStmt != nil {
		cond = p.makeExpr(condStmt, "boolean expression")
	} else if semi.pos.IsValid() {
		if semi.lit == "\n" {
			p.error(semi.pos, "unexpected newline, expecting { after if clause")
		} else {
			p.error(semi.pos, "missing condition in if statement")
		}
	}

	// make sure we have a valid AST
	if cond == nil {
		cond = &ast.BadExpr{From: p.pos, To: p.pos}
	}

	p.exprLev = prevLev
	return
}

func (p *parser) parseIfStmt() *ast.IfStmt {
	if p.trace {
		defer un(trace(p, "IfStmt"))
	}

	pos := p.expect(token.IF)

	init, cond := p.parseIfHeader()
	body := p.parseBlockStmt()

	var else_ ast.Stmt
	if p.tok == token.ELSE {
		p.next()
		switch p.tok {
		case token.IF:
			else_ = p.parseIfStmt()
		case token.LBRACE:
			else_ = p.parseBlockStmt()
			p.expectSemi()
		default:
			p.errorExpected(p.pos, "if statement or block")
			else_ = &ast.BadStmt{From: p.pos, To: p.pos}
		}
	} else {
		p.expectSemi()
	}

	return &ast.IfStmt{If: pos, Init: init, Cond: cond, Body: body, Else: else_}
}

func (p *parser) parseTypeList() (list []ast.Expr) {
	if p.trace {
		defer un(trace(p, "TypeList"))
	}

	list = append(list, p.parseType())
	for p.tok == token.COMMA {
		p.next()
		list = append(list, p.parseType())
	}

	return
}

func (p *parser) parseStmt() (s ast.Stmt) {
	if p.trace {
		defer un(trace(p, "Statement"))
	}

	switch p.tok {
	case token.CONST, token.TYPE, token.VAR:
		s = &ast.DeclStmt{Decl: p.parseDecl(stmtStart)}
	case
		// tokens that may start an expression
		token.IDENT, token.INT, token.FLOAT, token.IMAG, token.CHAR, token.STRING, token.FUN, token.LPAREN, // operands
		token.LBRACK,                                                     // composite types
		token.ADD, token.SUB, token.MUL, token.AND, token.XOR, token.NOT: // unary operators
		s, _ = p.parseSimpleStmt(labelOk)
		p.expectSemi()
	case token.RETURN:
		s = p.parseReturnStmt()
	case token.LBRACE:
		s = p.parseBlockStmt()
		p.expectSemi()
	case token.IF:
		s = p.parseIfStmt()
	case token.SEMICOLON:
		// Is it ever possible to have an implicit semicolon
		// producing an empty statement in a valid program?
		// (handle correctly anyway)
		s = &ast.EmptyStmt{Semicolon: p.pos, Implicit: p.lit == "\n"}
		p.next()
	case token.RBRACE:
		// a semicolon may be omitted before a closing "}"
		s = &ast.EmptyStmt{Semicolon: p.pos, Implicit: true}
	default:
		// no statement found
		pos := p.pos
		p.errorExpected(pos, "statement")
		p.advance(stmtStart)
		s = &ast.BadStmt{From: pos, To: p.pos}
	}

	return
}

// ----------------------------------------------------------------------------
// Declarations

type parseSpecFunction func(doc *ast.CommentGroup, pos token.Pos, keyword token.Token, iota int) ast.Spec

func isValidImport(lit string) bool {
	const illegalChars = `!"#$%&'()*,:;<=>?[\]^{|}` + "`\uFFFD"
	s, _ := strconv.Unquote(lit) // go/scanner returns a legal string literal
	for _, r := range s {
		if !unicode.IsGraphic(r) || unicode.IsSpace(r) || strings.ContainsRune(illegalChars, r) {
			return false
		}
	}
	return s != ""
}

func (p *parser) parseImportSpec(doc *ast.CommentGroup, _ token.Pos, _ token.Token, _ int) ast.Spec {
	if p.trace {
		defer un(trace(p, "ImportSpec"))
	}

	var ident *ast.Ident
	switch p.tok {
	case token.PERIOD:
		ident = &ast.Ident{NamePos: p.pos, Name: "."}
		p.next()
	case token.IDENT:
		ident = p.parseIdent()
	}

	pos := p.pos
	var path string
	if p.tok == token.STRING {
		path = p.lit
		if !isValidImport(path) {
			p.error(pos, "invalid import path: "+path)
		}
		p.next()
	} else {
		p.expect(token.STRING) // use expect() error handling
	}
	p.expectSemi() // call before accessing p.linecomment

	// collect imports
	spec := &ast.ImportSpec{
		Doc:     doc,
		Name:    ident,
		Path:    &ast.BasicLit{ValuePos: pos, Kind: token.STRING, Value: path},
		Comment: p.lineComment,
	}
	p.imports = append(p.imports, spec)

	return spec
}

func (p *parser) parseValueSpec(doc *ast.CommentGroup, _ token.Pos, keyword token.Token, iota int) ast.Spec {
	if p.trace {
		defer un(trace(p, keyword.String()+"Spec"))
	}

	pos := p.pos
	idents := p.parseIdentList()

	hasColon := false
	if p.tok == token.COLON {
		pos = p.pos
		p.next()
		hasColon = true
	}
	typ := p.tryIdentOrType()

	if typ != nil && !hasColon {
		p.error(pos, "expected \":\", got variable type")
	}

	var values []ast.Expr
	// always permit optional initialization for more tolerant parsing
	if p.tok == token.ASSIGN {
		p.next()
		values = p.parseList(true)
	}
	p.expectSemi() // call before accessing p.linecomment

	switch keyword {
	case token.VAR:
		if typ == nil && values == nil {
			p.error(pos, "missing variable type or initialization")
		}
	case token.CONST:
		if values == nil && (iota == 0 || typ != nil) {
			p.error(pos, "missing constant value")
		}
	}

	spec := &ast.ValueSpec{
		Doc:     doc,
		Names:   idents,
		Type:    typ,
		Values:  values,
		Comment: p.lineComment,
	}
	return spec
}

func (p *parser) parseGenericType(spec *ast.TypeSpec, openPos token.Pos, name0 *ast.Ident, closeTok token.Token) {
	list := p.parseParameterList(name0, closeTok, p.parseParamDecl, true)
	closePos := p.expect(closeTok)
	typeparams.Set(spec, &ast.FieldList{Opening: openPos, List: list, Closing: closePos})
	// Type alias cannot have type parameters. Accept them for robustness but complain.
	if p.tok == token.ASSIGN {
		p.error(p.pos, "generic type cannot be alias")
		p.next()
	}
	spec.Type = p.parseType()
}

func (p *parser) parseTypeSpec(doc *ast.CommentGroup, _ token.Pos, _ token.Token, _ int) ast.Spec {
	if p.trace {
		defer un(trace(p, "TypeSpec"))
	}

	ident := p.parseIdent()
	spec := &ast.TypeSpec{Doc: doc, Name: ident}

	switch p.tok {
	case token.LBRACK:
		lbrack := p.pos
		p.next()
		if p.tok == token.IDENT {
			// array type or generic type [T any]
			p.exprLev++
			x := p.parseExpr()
			p.exprLev--
			if name0, _ := x.(*ast.Ident); p.parseTypeParams() && name0 != nil && p.tok != token.RBRACK {
				// generic type [T any];
				p.parseGenericType(spec, lbrack, name0, token.RBRACK)
			}
		}

	default:
		// no type parameters
		if p.tok == token.ASSIGN {
			// type alias
			spec.Assign = p.pos
			p.next()
		}
		spec.Type = p.parseType()
	}

	p.expectSemi() // call before accessing p.linecomment
	spec.Comment = p.lineComment

	return spec
}

func (p *parser) parseGenDecl(keyword token.Token, f parseSpecFunction) *ast.GenDecl {
	if p.trace {
		defer un(trace(p, "GenDecl("+keyword.String()+")"))
	}

	doc := p.leadComment
	pos := p.expect(keyword)
	var lparen, rparen token.Pos
	var list []ast.Spec
	if p.tok == token.LPAREN {
		lparen = p.pos
		p.next()
		for iota := 0; p.tok != token.RPAREN && p.tok != token.EOF; iota++ {
			list = append(list, f(p.leadComment, pos, keyword, iota))
		}
		rparen = p.expect(token.RPAREN)
		p.expectSemi()
	} else {
		list = append(list, f(nil, pos, keyword, 0))
	}

	return &ast.GenDecl{
		Doc:    doc,
		TokPos: pos,
		Tok:    keyword,
		Lparen: lparen,
		Specs:  list,
		Rparen: rparen,
	}
}

func (p *parser) parseFuncDecl() *ast.FunDecl {
	if p.trace {
		defer un(trace(p, "FunctionDecl"))
	}

	doc := p.leadComment
	pos := p.expect(token.FUN)

	var recv *ast.FieldList
	if p.tok == token.LPAREN {
		_, recv = p.parseParameters(false)
	}

	ident := p.parseIdent()

	tparams, params := p.parseParameters(true)
	results := p.parseResult()

	var body *ast.BlockStmt
	if p.tok == token.LBRACE {
		body = p.parseBody()
		p.expectSemi()
	} else if p.tok == token.SEMICOLON {
		p.next()
		if p.tok == token.LBRACE {
			// opening { of function declaration on next line
			p.error(p.pos, "unexpected semicolon or newline before {")
			body = p.parseBody()
			p.expectSemi()
		}
	} else {
		p.expectSemi()
	}

	decl := &ast.FunDecl{
		Doc:  doc,
		Recv: recv,
		Name: ident,
		Type: &ast.FunType{
			Fun:     pos,
			Params:  params,
			Results: results,
		},
		Body: body,
	}
	typeparams.Set(decl.Type, tparams)
	return decl
}

func (p *parser) parseDecl(sync map[token.Token]bool) ast.Decl {
	if p.trace {
		defer un(trace(p, "Declaration"))
	}

	var f parseSpecFunction
	switch p.tok {
	case token.CONST, token.VAR:
		f = p.parseValueSpec

	case token.TYPE:
		f = p.parseTypeSpec

	case token.FUN:
		return p.parseFuncDecl()

	default:
		pos := p.pos
		p.errorExpected(pos, "declaration")
		p.advance(sync)
		return &ast.BadDecl{From: pos, To: p.pos}
	}

	return p.parseGenDecl(p.tok, f)
}

// ----------------------------------------------------------------------------
// Source files

func (p *parser) parseFile() *ast.File {
	if p.trace {
		defer un(trace(p, "File"))
	}

	// Don't bother parsing the rest if we had errors scanning the first token.
	// Likely not a Go source file at all.
	if p.errors.Len() != 0 {
		return nil
	}

	// package clause
	doc := p.leadComment
	pos := p.expect(token.PACKAGE)
	// Go spec: The package clause is not a declaration;
	// the package name does not appear in any scope.
	ident := p.parseIdent()
	if ident.Name == "_" && p.mode&DeclarationErrors != 0 {
		p.error(p.pos, "invalid package name _")
	}
	p.expectSemi()

	// Don't bother parsing the rest if we had errors parsing the package clause.
	// Likely not a Go source file at all.
	if p.errors.Len() != 0 {
		return nil
	}

	var decls []ast.Decl
	if p.mode&PackageClauseOnly == 0 {
		// import decls
		for p.tok == token.IMPORT {
			decls = append(decls, p.parseGenDecl(token.IMPORT, p.parseImportSpec))
		}

		if p.mode&ImportsOnly == 0 {
			// rest of package body
			for p.tok != token.EOF {
				decls = append(decls, p.parseDecl(declStart))
			}
		}
	}

	f := &ast.File{
		Doc:      doc,
		Package:  pos,
		Name:     ident,
		Decls:    decls,
		Imports:  p.imports,
		Comments: p.comments,
	}
	var declErr func(token.Pos, string)
	if p.mode&DeclarationErrors != 0 {
		declErr = p.error
	}
	if p.mode&SkipObjectResolution == 0 {
		resolveFile(f, p.file, declErr)
	}

	return f
}
