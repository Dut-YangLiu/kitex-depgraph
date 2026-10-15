package scan

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"

	"github.com/Dut-YangLiu/kitex-depgraph/internal/model"
)

// pkgAnalyzer extracts facts from one type-checked workspace package:
// call-graph nodes and edges, Kitex client creations, server registrations,
// RPC call sites and registered service names.
type pkgAnalyzer struct {
	s      *scanner
	p      *lpkg
	info   *types.Info
	repo   *repo
	initID string
}

func (s *scanner) analyzePackage(p *lpkg) {
	a := &pkgAnalyzer{s: s, p: p, info: p.info, repo: p.pd.repo}
	for _, f := range p.files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				obj, _ := a.info.Defs[d.Name].(*types.Func)
				if obj == nil {
					continue
				}
				id := a.declare(obj, d)
				ast.Walk(&walker{a: a, fnID: id, fnObj: obj, skip: map[ast.Node]bool{}}, d.Body)
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || len(vs.Values) == 0 {
						continue
					}
					ast.Walk(&walker{a: a, fnID: a.initFunc(vs), skip: map[ast.Node]bool{}}, vs)
				}
			}
		}
	}
}

func (a *pkgAnalyzer) declare(obj *types.Func, d *ast.FuncDecl) string {
	id := a.s.funcID(a.p, obj)
	name := shortFuncName(a.p, obj)
	file, line := a.s.position(a.repo, d.Pos())
	_, end := a.s.position(a.repo, d.End())
	if obj.Name() == "init" && obj.Type().(*types.Signature).Recv() == nil {
		id = fmt.Sprintf("%s#%d", id, line)
		name = fmt.Sprintf("%s#%d", name, line)
	}
	a.s.addFunc(&funcFact{id: id, repo: a.repo, pd: a.p.pd, name: name, file: file, line: line, end: end})
	return id
}

// initFunc is a pseudo function holding package-level variable initializers.
func (a *pkgAnalyzer) initFunc(vs *ast.ValueSpec) string {
	if a.initID != "" {
		return a.initID
	}
	a.initID = a.repo.name + ":" + a.p.importPath + ".init·vars"
	file, line := a.s.position(a.repo, vs.Pos())
	a.s.addFunc(&funcFact{id: a.initID, repo: a.repo, pd: a.p.pd, name: relPkgPath(a.p) + ".<包级变量初始化>", file: file, line: line, end: line})
	return a.initID
}

type walker struct {
	a     *pkgAnalyzer
	fnID  string
	fnObj *types.Func
	stack []ast.Node
	skip  map[ast.Node]bool
}

func (w *walker) Visit(n ast.Node) ast.Visitor {
	if n == nil {
		w.stack = w.stack[:len(w.stack)-1]
		return nil
	}
	w.stack = append(w.stack, n)
	switch x := n.(type) {
	case *ast.CallExpr:
		w.call(x)
	case *ast.SelectorExpr:
		if !w.skip[x] {
			w.refSelector(x)
		}
	case *ast.Ident:
		if !w.skip[x] {
			w.refIdent(x)
		}
	}
	return w
}

func (w *walker) line(n ast.Node) int { return w.a.s.l.fset.Position(n.Pos()).Line }

func (w *walker) call(x *ast.CallExpr) {
	fun := unparen(x.Fun)
	switch ix := fun.(type) {
	case *ast.IndexExpr:
		fun = unparen(ix.X)
	case *ast.IndexListExpr:
		fun = unparen(ix.X)
	}
	w.skip[fun] = true
	var callee *types.Func
	switch f := fun.(type) {
	case *ast.Ident:
		if fn, ok := w.a.info.Uses[f].(*types.Func); ok {
			callee = fn
			w.static(fn, x)
		}
	case *ast.SelectorExpr:
		w.skip[f.Sel] = true
		if f.Sel.Name == "WithServerBasicInfo" {
			w.serverBasicInfo(x)
		}
		if sel := w.a.info.Selections[f]; sel != nil {
			fn, ok := sel.Obj().(*types.Func)
			if !ok {
				break // calling a function-typed field
			}
			callee = fn
			if sel.Kind() == types.MethodVal && isInterfaceMethod(fn) {
				w.iface(f, sel, fn, x)
			} else {
				w.static(fn, x)
			}
		} else if fn, ok := w.a.info.Uses[f.Sel].(*types.Func); ok {
			callee = fn
			w.static(fn, x)
		}
	}
	// A helper returning a Kitex client (newUserClient(), wrapper.KitexClient())
	// passes its destService on to whatever stores the result.
	if callee != nil && !w.a.s.isKitexCtor(callee) && w.returnsKitexClient(x) {
		w.flow(x, holderSrc{fn: callee.Origin()})
	}
}

func (w *walker) static(fn *types.Func, call *ast.CallExpr) {
	fn = fn.Origin()
	cp := w.a.s.l.byTypes[fn.Pkg()]
	if cp == nil {
		return
	}
	if cp.svc != nil {
		switch fn.Name() {
		case "NewClient", "MustNewClient":
			w.clientInit(cp, call)
		case "NewServer":
			w.serverReg(cp, fn, call, 0)
		case "RegisterService":
			w.serverReg(cp, fn, call, 1)
		}
		return
	}
	if to := w.a.s.funcIDOf(fn); to != "" {
		w.a.s.addEdge(w.fnID, to, "", "call", w.line(call))
	}
}

func (w *walker) iface(selExpr *ast.SelectorExpr, sel *types.Selection, fn *types.Func, call *ast.CallExpr) {
	if cp := w.a.s.clientIfacePkg(fn); cp != nil {
		w.rpc(selExpr, fn, cp, call)
		return
	}
	it, _ := sel.Recv().Underlying().(*types.Interface)
	if it == nil {
		return
	}
	w.a.s.pending = append(w.a.s.pending, &ifaceFact{
		from: w.fnID, repo: w.a.repo, declRepo: w.a.s.declRepo(sel.Recv()),
		iface: it, method: fn.Name(), line: w.line(call),
	})
}

func (w *walker) rpc(selExpr *ast.SelectorExpr, fn *types.Func, cp *lpkg, call *ast.CallExpr) {
	w.a.s.callSeq++
	cf := &callFact{
		id:     fmt.Sprintf("c%d", w.a.s.callSeq),
		repo:   w.a.repo,
		pd:     w.a.p.pd,
		fn:     w.fnID,
		svc:    cp,
		method: fn.Name(),
		wire:   cp.svc.wire(fn.Name()),
	}
	cf.file, cf.line = w.a.s.position(w.a.repo, call.Pos())
	cf.holderObj, cf.holderFn = w.holderOf(selExpr.X)
	w.a.s.calls = append(w.a.s.calls, cf)
	w.a.s.addEdge(w.fnID, "", cf.id, "rpc", cf.line)
}

// holderOf finds the variable, field or factory function a client value
// comes from, so its destService can be looked up.
func (w *walker) holderOf(x ast.Expr) (types.Object, *types.Func) {
	switch e := unparen(x).(type) {
	case *ast.Ident:
		if v, ok := w.a.info.Uses[e].(*types.Var); ok {
			return v.Origin(), nil
		}
	case *ast.SelectorExpr:
		if sel := w.a.info.Selections[e]; sel != nil {
			if v, ok := sel.Obj().(*types.Var); ok {
				return v.Origin(), nil
			}
			return nil, nil
		}
		if v, ok := w.a.info.Uses[e.Sel].(*types.Var); ok {
			return v.Origin(), nil
		}
	case *ast.CallExpr:
		if fn := w.calleeOf(e); fn != nil {
			return nil, fn.Origin()
		}
	case *ast.IndexExpr:
		return w.holderOf(e.X)
	case *ast.StarExpr:
		return w.holderOf(e.X)
	}
	return nil, nil
}

func (w *walker) calleeOf(call *ast.CallExpr) *types.Func {
	fun := unparen(call.Fun)
	switch ix := fun.(type) {
	case *ast.IndexExpr:
		fun = unparen(ix.X)
	case *ast.IndexListExpr:
		fun = unparen(ix.X)
	}
	switch f := fun.(type) {
	case *ast.Ident:
		fn, _ := w.a.info.Uses[f].(*types.Func)
		return fn
	case *ast.SelectorExpr:
		if sel := w.a.info.Selections[f]; sel != nil {
			fn, _ := sel.Obj().(*types.Func)
			return fn
		}
		fn, _ := w.a.info.Uses[f.Sel].(*types.Func)
		return fn
	}
	return nil
}

func (w *walker) returnsKitexClient(call *ast.CallExpr) bool {
	t := w.a.info.TypeOf(call)
	if t == nil {
		return false
	}
	if tup, ok := t.(*types.Tuple); ok {
		if tup.Len() == 0 {
			return false
		}
		t = tup.At(0).Type()
	}
	return w.a.s.kitexClientType(t) != nil
}

func (w *walker) refIdent(id *ast.Ident) {
	fn, ok := w.a.info.Uses[id].(*types.Func)
	if !ok || isInterfaceMethod(fn) {
		return
	}
	w.addRef(fn, id)
}

func (w *walker) refSelector(x *ast.SelectorExpr) {
	w.skip[x.Sel] = true
	if sel := w.a.info.Selections[x]; sel != nil {
		if sel.Kind() == types.FieldVal {
			return
		}
		if fn, ok := sel.Obj().(*types.Func); ok && !isInterfaceMethod(fn) {
			w.addRef(fn, x)
		}
		return
	}
	if fn, ok := w.a.info.Uses[x.Sel].(*types.Func); ok {
		w.addRef(fn, x)
	}
}

func (w *walker) addRef(fn *types.Func, at ast.Node) {
	if to := w.a.s.funcIDOf(fn.Origin()); to != "" && to != w.fnID {
		w.a.s.addEdge(w.fnID, to, "", "ref", w.line(at))
	}
}

func (w *walker) clientInit(cp *lpkg, call *ast.CallExpr) {
	cf := &clientFact{repo: w.a.repo, pd: w.a.p.pd, svc: cp, dest: model.Dest{Kind: "unknown"}}
	cf.file, cf.line = w.a.s.position(w.a.repo, call.Pos())
	if len(call.Args) > 0 {
		cf.dest = w.evalDest(call.Args[0])
	}
	w.a.s.clients = append(w.a.s.clients, cf)
	cf.holder = w.flow(call, holderSrc{client: cf})
}

// flow records where the client produced by call ends up: a variable, a
// struct field, or the enclosing function's return value.
func (w *walker) flow(call ast.Node, src holderSrc) string {
	parent, child := w.parentOf(call)
	s := w.a.s
	switch p := parent.(type) {
	case *ast.AssignStmt:
		idx := indexOfExpr(p.Rhs, child)
		var lhs ast.Expr
		if len(p.Lhs) == len(p.Rhs) && idx >= 0 {
			lhs = p.Lhs[idx]
		} else if idx == 0 && len(p.Lhs) > 0 {
			lhs = p.Lhs[0]
		}
		if obj := w.lhsObj(lhs); obj != nil {
			s.holders[obj] = append(s.holders[obj], src)
			return s.objDesc(obj)
		}
	case *ast.ValueSpec:
		idx := indexOfExpr(p.Values, child)
		var name *ast.Ident
		if len(p.Names) == len(p.Values) && idx >= 0 {
			name = p.Names[idx]
		} else if len(p.Names) > 0 {
			name = p.Names[0]
		}
		if name != nil {
			if v, ok := w.a.info.Defs[name].(*types.Var); ok {
				o := v.Origin()
				s.holders[o] = append(s.holders[o], src)
				return s.objDesc(o)
			}
		}
	case *ast.KeyValueExpr:
		if key, ok := p.Key.(*ast.Ident); ok && p.Value == child {
			if v, ok := w.a.info.Uses[key].(*types.Var); ok && v.IsField() {
				o := v.Origin()
				s.holders[o] = append(s.holders[o], src)
				return s.objDesc(o)
			}
		}
	case *ast.ReturnStmt:
		if w.fnObj != nil {
			f := w.fnObj.Origin()
			s.factories[f] = append(s.factories[f], src)
			return "返回值 " + shortFuncName(w.a.p, f)
		}
	}
	return ""
}

func (w *walker) parentOf(n ast.Node) (ast.Node, ast.Node) {
	child := n
	for i := len(w.stack) - 2; i >= 0; i-- {
		if pe, ok := w.stack[i].(*ast.ParenExpr); ok {
			child = pe
			continue
		}
		return w.stack[i], child
	}
	return nil, nil
}

func indexOfExpr(list []ast.Expr, n ast.Node) int {
	for i, e := range list {
		if ast.Node(e) == n {
			return i
		}
	}
	return -1
}

func (w *walker) lhsObj(lhs ast.Expr) types.Object {
	switch e := unparen(lhs).(type) {
	case *ast.Ident:
		if e.Name == "_" {
			return nil
		}
		if v, ok := w.a.info.Defs[e].(*types.Var); ok {
			return v.Origin()
		}
		if v, ok := w.a.info.Uses[e].(*types.Var); ok {
			return v.Origin()
		}
	case *ast.SelectorExpr:
		if sel := w.a.info.Selections[e]; sel != nil {
			if v, ok := sel.Obj().(*types.Var); ok {
				return v.Origin()
			}
			return nil
		}
		if v, ok := w.a.info.Uses[e.Sel].(*types.Var); ok {
			return v.Origin()
		}
	case *ast.IndexExpr:
		return w.lhsObj(e.X)
	case *ast.StarExpr:
		return w.lhsObj(e.X)
	}
	return nil
}

// evalDest evaluates a destService / ServiceName expression.
func (w *walker) evalDest(e ast.Expr) model.Dest {
	if tv, ok := w.a.info.Types[e]; ok && tv.Value != nil && tv.Value.Kind() == constant.String {
		return model.Dest{Value: constant.StringVal(tv.Value), Kind: "const"}
	}
	if s, ok := stringLit(e); ok {
		return model.Dest{Value: s, Kind: "const"}
	}
	var obj types.Object
	switch x := unparen(e).(type) {
	case *ast.Ident:
		obj = w.a.info.Uses[x]
	case *ast.SelectorExpr:
		if w.a.info.Selections[x] == nil {
			obj = w.a.info.Uses[x.Sel]
		}
	}
	if v, ok := obj.(*types.Var); ok {
		if s, ok := w.a.s.strVars[v]; ok {
			return model.Dest{Value: s, Kind: "var", Expr: types.ExprString(e)}
		}
	}
	if c, ok := obj.(*types.Const); ok && c.Val().Kind() == constant.String {
		return model.Dest{Value: constant.StringVal(c.Val()), Kind: "const"}
	}
	return model.Dest{Kind: "dynamic", Expr: types.ExprString(e)}
}

// serverBasicInfo records server.WithServerBasicInfo(&rpcinfo.EndpointBasicInfo{ServiceName: X}).
// The Kitex packages are stand-ins, so this is matched by name.
func (w *walker) serverBasicInfo(call *ast.CallExpr) {
	for _, arg := range call.Args {
		a := unparen(arg)
		if u, ok := a.(*ast.UnaryExpr); ok {
			a = unparen(u.X)
		}
		cl, ok := a.(*ast.CompositeLit)
		if !ok {
			continue
		}
		for _, elt := range cl.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "ServiceName" {
				nf := &nameFact{repo: w.a.repo, pd: w.a.p.pd, dest: w.evalDest(kv.Value)}
				nf.file, nf.line = w.a.s.position(w.a.repo, kv.Pos())
				w.a.s.names = append(w.a.s.names, nf)
			}
		}
	}
}

func (w *walker) serverReg(cp *lpkg, fn *types.Func, call *ast.CallExpr, idx int) {
	sf := &serverFact{repo: w.a.repo, pd: w.a.p.pd, svc: cp}
	sf.file, sf.line = w.a.s.position(w.a.repo, call.Pos())
	if idx < len(call.Args) {
		h := call.Args[idx]
		sf.handlerExpr = types.ExprString(h)
		if named := concreteNamed(w.a.info.TypeOf(h)); named != nil {
			sf.handler = named
			sf.via = "static"
		} else if sig, ok := fn.Type().(*types.Signature); ok && sig.Params().Len() > idx {
			sf.handlerIface = sig.Params().At(idx).Type()
		}
	}
	w.a.s.servers = append(w.a.s.servers, sf)
}

func concreteNamed(t types.Type) *types.Named {
	if t == nil {
		return nil
	}
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || types.IsInterface(named) {
		return nil
	}
	return named.Origin()
}

func isInterfaceMethod(fn *types.Func) bool {
	sig, ok := fn.Type().(*types.Signature)
	return ok && sig.Recv() != nil && types.IsInterface(sig.Recv().Type())
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// relPkgPath is the package path relative to its module ("biz/service"), or
// the package name for the module root package.
func relPkgPath(p *lpkg) string {
	if p.pd != nil {
		if rest, ok := cutModPrefix(p.importPath, p.pd.mod.path); ok && rest != "" {
			return rest
		}
	}
	return p.name
}

func shortFuncName(p *lpkg, fn *types.Func) string {
	rel := relPkgPath(p)
	sig, _ := fn.Type().(*types.Signature)
	if sig != nil && sig.Recv() != nil {
		t := sig.Recv().Type()
		star := ""
		if pt, ok := t.(*types.Pointer); ok {
			t = pt.Elem()
			star = "*"
		}
		tn := "?"
		if n, ok := t.(*types.Named); ok {
			tn = n.Obj().Name()
		}
		return fmt.Sprintf("(%s%s.%s).%s", star, rel, tn, fn.Name())
	}
	return rel + "." + fn.Name()
}
