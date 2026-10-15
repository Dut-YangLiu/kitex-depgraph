// Package scan extracts Kitex service facts and an intra-repo call graph from
// a set of repositories, without building them.
package scan

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Dut-YangLiu/kitex-depgraph/internal/model"
)

// Options control a scan.
type Options struct {
	Verbose bool
	Log     io.Writer
}

// chaLimit caps how many implementations an interface call may fan out to.
const chaLimit = 20

type holderSrc struct {
	client *clientFact // a NewClient call
	fn     *types.Func // a function returning a client
}

type funcFact struct {
	id         string
	repo       *repo
	pd         *pkgDir
	name, file string
	line, end  int
}

type edgeKey struct{ from, to, call, kind string }

type callFact struct {
	id           string
	repo         *repo
	pd           *pkgDir
	fn           string
	file         string
	line         int
	svc          *lpkg
	method, wire string
	holderObj    types.Object
	holderFn     *types.Func
	dest         model.Dest
}

type clientFact struct {
	repo   *repo
	pd     *pkgDir
	svc    *lpkg
	dest   model.Dest
	holder string
	file   string
	line   int
}

type serverFact struct {
	repo         *repo
	pd           *pkgDir
	svc          *lpkg
	file         string
	line         int
	handler      *types.Named
	handlerIface types.Type
	handlerExpr  string
	via          string
	cands        int
	methods      map[string]string
}

type nameFact struct {
	repo *repo
	pd   *pkgDir
	dest model.Dest
	file string
	line int
}

type ifaceFact struct {
	from     string
	repo     *repo
	declRepo *repo // repo declaring the interface, if in the workspace
	iface    *types.Interface
	method   string
	line     int
}

type typeEntry struct {
	named   *types.Named
	methods map[string]*types.Func
}

type implKey struct {
	repos string
	iface *types.Interface
}

type scanner struct {
	cfg  *Config
	ws   *workspace
	l    *loader
	opts Options

	funcs     map[string]*funcFact
	edges     []model.Edge
	edgeSeen  map[edgeKey]bool
	calls     []*callFact
	clients   []*clientFact
	servers   []*serverFact
	names     []*nameFact
	pending   []*ifaceFact
	holders   map[types.Object][]holderSrc
	factories map[*types.Func][]holderSrc
	strVars   map[*types.Var]string
	repoTypes map[*repo][]typeEntry
	implCache map[implKey][]*types.Named
	compName  map[string]string // package dir -> component name
	compKind  map[string]string // component name -> service | library
	compDir   map[string]string // component name -> absolute dir
	compRepo  map[string]*repo  // component name -> repo
	callSeq   int
	chaCapped int
}

// Scan scans the workspace. The returned graph is not linked yet: call
// link.Link on it. Warnings are non-fatal problems worth showing.
func Scan(cfg *Config, opts Options) (*model.Graph, []string, error) {
	start := time.Now()
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	ws, err := discover(cfg)
	if err != nil {
		return nil, nil, err
	}
	l := newLoader(ws, cfg)
	l.log, l.verbose = opts.Log, opts.Verbose
	s := &scanner{
		cfg: cfg, ws: ws, l: l, opts: opts,
		funcs:     map[string]*funcFact{},
		edgeSeen:  map[edgeKey]bool{},
		holders:   map[types.Object][]holderSrc{},
		factories: map[*types.Func][]holderSrc{},
		strVars:   map[*types.Var]string{},
		repoTypes: map[*repo][]typeEntry{},
		implCache: map[implKey][]*types.Named{},
		compName:  map[string]string{},
		compKind:  map[string]string{},
		compDir:   map[string]string{},
		compRepo:  map[string]*repo{},
	}
	l.onLoaded = s.preindex
	for _, r := range ws.repos {
		fmt.Fprintf(opts.Log, "扫描仓库 %s(%d 个包)\n", r.name, len(r.pkgDirs))
		pds := append([]*pkgDir(nil), r.pkgDirs...)
		sort.Slice(pds, func(i, j int) bool { return pds[i].importPath < pds[j].importPath })
		for _, pd := range pds {
			p := l.load(pd.dir, pd.importPath, "workspace", pd.mod)
			if p.state == stateDone && p.info != nil {
				s.analyzePackage(p)
			}
			p.info, p.files = nil, nil // keep only the type information
		}
	}
	s.resolveInterfaceCalls()
	s.resolveServers()
	s.resolveDests()
	s.assignComponents()
	g := s.build()
	g.Stats.Seconds = float64(time.Since(start).Milliseconds()) / 1000
	warnings := append([]string(nil), ws.warnings...)
	if len(l.missingGen) > 0 {
		paths := model.SortedKeys(l.missingGen)
		var lines []string
		for i, p := range paths {
			if i >= 10 {
				lines = append(lines, fmt.Sprintf("    …… 共 %d 个", len(paths)))
				break
			}
			lines = append(lines, fmt.Sprintf("    %s(被 %s 引用)", p, strings.Join(l.missingGen[p], "、")))
		}
		warnings = append(warnings, fmt.Sprintf("有 %d 个 Kitex 生成代码包找不到源码，经由它们的 RPC 调用无法识别:\n%s\n  解决办法：把公共 IDL 仓库加入工作区配置，或在对应仓库里执行 go mod download(或 go mod vendor)。",
			len(paths), strings.Join(lines, "\n")))
	}
	if opts.Verbose {
		s.logTypeErrors()
	}
	return g, warnings, nil
}

// logTypeErrors prints the packages with the most type-checking errors.
// Errors are expected (imports outside the workspace are stand-ins); a
// workspace package that fails badly usually points to a missing repo.
func (s *scanner) logTypeErrors() {
	var ps []*lpkg
	for _, p := range s.l.pkgs {
		if p.pd != nil && p.errCount > 0 {
			ps = append(ps, p)
		}
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].errCount > ps[j].errCount })
	fmt.Fprintf(s.opts.Log, "类型检查错误最多的包(依赖用占位包代替时出现错误属正常):\n")
	for i, p := range ps {
		if i >= 15 {
			break
		}
		sample := ""
		if len(p.errSample) > 0 {
			sample = p.errSample[0]
		}
		fmt.Fprintf(s.opts.Log, "  %5d  %s\n         例:%s\n", p.errCount, p.importPath, sample)
	}
	fmt.Fprintf(s.opts.Log, "占位包 %d 个，从工作区外加载源码的包 %d 个\n", len(s.l.fakes), countExternal(s.l))
}

func countExternal(l *loader) int {
	n := 0
	for _, p := range l.pkgs {
		if p.pd == nil && p.state == stateDone {
			n++
		}
	}
	return n
}

// preindex runs right after a workspace package is type-checked, before any
// package importing it is analysed: it remembers string variables with a
// literal initializer, which are a common way to hold service names.
func (s *scanner) preindex(p *lpkg) {
	if p.pd == nil || p.info == nil {
		if p.svc != nil || p.pd == nil {
			p.files = nil // generated or external code: types are enough from now on
		}
		return
	}
	for _, f := range p.files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Values) != len(vs.Names) {
					continue
				}
				for i, n := range vs.Names {
					if str, ok := stringLit(vs.Values[i]); ok {
						if v, ok := p.info.Defs[n].(*types.Var); ok {
							s.strVars[v] = str
						}
					}
				}
			}
		}
	}
}

func (s *scanner) funcID(p *lpkg, fn *types.Func) string {
	return p.pd.repo.name + ":" + fn.FullName()
}

// funcIDOf returns the call-graph ID of a function declared in the
// workspace, or "" for anything else.
func (s *scanner) funcIDOf(fn *types.Func) string {
	p := s.l.byTypes[fn.Pkg()]
	if p == nil || p.pd == nil || p.gen.kitex {
		return ""
	}
	return s.funcID(p, fn)
}

func (s *scanner) addFunc(f *funcFact) {
	if _, ok := s.funcs[f.id]; !ok {
		s.funcs[f.id] = f
	}
}

func (s *scanner) addEdge(from, to, call, kind string, line int) {
	k := edgeKey{from, to, call, kind}
	if s.edgeSeen[k] {
		return
	}
	s.edgeSeen[k] = true
	s.edges = append(s.edges, model.Edge{From: from, To: to, Call: call, Kind: kind, Line: line})
}

func (s *scanner) position(r *repo, pos token.Pos) (string, int) {
	p := s.l.fset.Position(pos)
	rel, err := filepath.Rel(r.dir, p.Filename)
	if err != nil {
		rel = p.Filename
	}
	return filepath.ToSlash(rel), p.Line
}

func (s *scanner) isKitexCtor(fn *types.Func) bool {
	p := s.l.byTypes[fn.Pkg()]
	return p != nil && p.svc != nil && (fn.Name() == "NewClient" || fn.Name() == "MustNewClient")
}

// kitexClientType returns the service package when t is a generated Client.
func (s *scanner) kitexClientType(t types.Type) *lpkg {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Name() != "Client" {
		return nil
	}
	p := s.l.byTypes[named.Obj().Pkg()]
	if p == nil || p.svc == nil {
		return nil
	}
	return p
}

// clientIfacePkg returns the service package when fn is a method of a
// generated Kitex Client interface.
func (s *scanner) clientIfacePkg(fn *types.Func) *lpkg {
	p := s.l.byTypes[fn.Pkg()]
	if p == nil || p.svc == nil {
		return nil
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return nil
	}
	if named, ok := sig.Recv().Type().(*types.Named); ok && named.Obj().Name() != "Client" {
		return nil // e.g. a streaming interface
	}
	if len(p.svc.clientMethods) > 0 && !p.svc.clientMethods[fn.Name()] {
		return nil
	}
	return p
}

func (s *scanner) objDesc(obj types.Object) string {
	v, ok := obj.(*types.Var)
	if !ok {
		return obj.Name()
	}
	switch {
	case v.IsField():
		return "字段 " + v.Name()
	case v.Parent() != nil && v.Pkg() != nil && v.Parent() == v.Pkg().Scope():
		if p := s.l.byTypes[v.Pkg()]; p != nil {
			return "包变量 " + relPkgPath(p) + "." + v.Name()
		}
		return "包变量 " + v.Name()
	default:
		return "局部变量 " + v.Name()
	}
}

// typesOf lists the concrete named types declared in a repo with their
// method names (value and pointer receivers).
func (s *scanner) typesOf(r *repo) []typeEntry {
	if ts, ok := s.repoTypes[r]; ok {
		return ts
	}
	var out []typeEntry
	for _, p := range s.l.pkgs {
		if p.pd == nil || p.pd.repo != r || p.gen.kitex || p.types == nil {
			continue
		}
		scope := p.types.Scope()
		for _, name := range scope.Names() {
			tn, ok := scope.Lookup(name).(*types.TypeName)
			if !ok || tn.IsAlias() {
				continue
			}
			named, ok := tn.Type().(*types.Named)
			if !ok || types.IsInterface(named) {
				continue
			}
			ms := types.NewMethodSet(types.NewPointer(named))
			if ms.Len() == 0 {
				continue
			}
			methods := make(map[string]*types.Func, ms.Len())
			for i := 0; i < ms.Len(); i++ {
				if fn, ok := ms.At(i).Obj().(*types.Func); ok {
					methods[fn.Name()] = fn
				}
			}
			out = append(out, typeEntry{named: named, methods: methods})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].named.Obj(), out[j].named.Obj()
		if a.Pkg().Path() != b.Pkg().Path() {
			return a.Pkg().Path() < b.Pkg().Path()
		}
		return a.Name() < b.Name()
	})
	s.repoTypes[r] = out
	return out
}

// implementers is class hierarchy analysis tolerant of stand-in packages:
// a type implements an interface when it has a method for every interface
// method name, with a compatible signature. Parameter and result types are
// compared only where both sides are known (stand-in packages leave some
// types invalid).
func (s *scanner) implementers(repos []*repo, it *types.Interface) []*types.Named {
	var names []string
	for _, r := range repos {
		names = append(names, r.name)
	}
	k := implKey{strings.Join(names, ","), it}
	if c, ok := s.implCache[k]; ok {
		return c
	}
	var out []*types.Named
	n := it.NumMethods()
	trivial := n == 0 || (n == 1 && (it.Method(0).Name() == "Error" || it.Method(0).Name() == "String"))
	if !trivial {
		for _, r := range repos {
			for _, te := range s.typesOf(r) {
				ok := true
				for i := 0; i < n && ok; i++ {
					im := it.Method(i)
					fn := te.methods[im.Name()]
					ok = fn != nil && compatibleSig(im.Type().(*types.Signature), fn.Type().(*types.Signature))
				}
				if ok {
					out = append(out, te.named)
				}
			}
		}
	}
	s.implCache[k] = out
	return out
}

func compatibleSig(a, b *types.Signature) bool {
	if a.Params().Len() != b.Params().Len() || a.Results().Len() != b.Results().Len() {
		return false
	}
	for i := 0; i < a.Params().Len(); i++ {
		if !compatibleType(a.Params().At(i).Type(), b.Params().At(i).Type()) {
			return false
		}
	}
	for i := 0; i < a.Results().Len(); i++ {
		if !compatibleType(a.Results().At(i).Type(), b.Results().At(i).Type()) {
			return false
		}
	}
	return true
}

func compatibleType(x, y types.Type) bool {
	if hasInvalid(x) || hasInvalid(y) {
		return true
	}
	return types.Identical(x, y)
}

func hasInvalid(t types.Type) bool {
	switch x := t.(type) {
	case *types.Basic:
		return x.Kind() == types.Invalid
	case *types.Pointer:
		return hasInvalid(x.Elem())
	case *types.Slice:
		return hasInvalid(x.Elem())
	case *types.Array:
		return hasInvalid(x.Elem())
	case *types.Map:
		return hasInvalid(x.Key()) || hasInvalid(x.Elem())
	case *types.Chan:
		return hasInvalid(x.Elem())
	case *types.Named:
		return x.Underlying() == nil || hasInvalidUnderlying(x)
	}
	return false
}

func hasInvalidUnderlying(n *types.Named) bool {
	b, ok := n.Underlying().(*types.Basic)
	return ok && b.Kind() == types.Invalid
}

// declRepo returns the workspace repo declaring a (named) type.
func (s *scanner) declRepo(t types.Type) *repo {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return nil
	}
	if p := s.l.byTypes[named.Obj().Pkg()]; p != nil && p.pd != nil {
		return p.pd.repo
	}
	return nil
}

func scopeRepos(r, decl *repo) []*repo {
	if decl == nil || decl == r {
		return []*repo{r}
	}
	return []*repo{r, decl}
}

func (s *scanner) resolveInterfaceCalls() {
	for _, pf := range s.pending {
		cands := s.implementers(scopeRepos(pf.repo, pf.declRepo), pf.iface)
		if len(cands) > chaLimit {
			s.chaCapped++
			continue
		}
		for _, named := range cands {
			obj, _, _ := types.LookupFieldOrMethod(types.NewPointer(named), true, named.Obj().Pkg(), pf.method)
			if fn, ok := obj.(*types.Func); ok {
				if to := s.funcIDOf(fn.Origin()); to != "" && to != pf.from {
					s.addEdge(pf.from, to, "", "iface", pf.line)
				}
			}
		}
	}
}

func (s *scanner) resolveServers() {
	for _, sf := range s.servers {
		if sf.handler == nil && sf.handlerIface != nil {
			if it, ok := sf.handlerIface.Underlying().(*types.Interface); ok {
				cands := s.implementers([]*repo{sf.repo}, it)
				var same []*types.Named
				for _, c := range cands {
					if p := s.l.byTypes[c.Obj().Pkg()]; p != nil && p.pd != nil && p.pd.mod == sf.pd.mod {
						same = append(same, c)
					}
				}
				switch {
				case len(same) == 1:
					sf.handler, sf.via = same[0], "cha"
				case len(cands) == 1:
					sf.handler, sf.via = cands[0], "cha"
				default:
					sf.cands = len(cands)
				}
			}
		}
		if sf.handler == nil {
			sf.via = "unknown"
		}
		sf.methods = map[string]string{}
		if sf.handler == nil {
			continue
		}
		for _, m := range sf.svc.svc.methods {
			obj, _, _ := types.LookupFieldOrMethod(types.NewPointer(sf.handler), true, sf.handler.Obj().Pkg(), m.Go)
			if fn, ok := obj.(*types.Func); ok {
				if id := s.funcIDOf(fn.Origin()); id != "" {
					sf.methods[m.Wire] = id
				}
			}
		}
	}
}

// resolveDests follows each call's client back to the NewClient call(s) that
// produced it.
func (s *scanner) resolveDests() {
	for _, c := range s.calls {
		c.dest = combineDests(s.destsOf(c.holderObj, c.holderFn))
	}
}

func (s *scanner) destsOf(obj types.Object, fn *types.Func) []model.Dest {
	seen := map[any]bool{}
	var out []model.Dest
	var visitObj func(types.Object)
	var visitFn func(*types.Func)
	visit := func(src holderSrc) {
		if src.client != nil {
			out = append(out, src.client.dest)
		} else if src.fn != nil {
			visitFn(src.fn)
		}
	}
	visitObj = func(o types.Object) {
		if seen[o] {
			return
		}
		seen[o] = true
		for _, src := range s.holders[o] {
			visit(src)
		}
	}
	visitFn = func(f *types.Func) {
		if seen[f] {
			return
		}
		seen[f] = true
		for _, src := range s.factories[f] {
			visit(src)
		}
	}
	if obj != nil {
		visitObj(obj)
	}
	if fn != nil {
		visitFn(fn)
	}
	return out
}

func combineDests(ds []model.Dest) model.Dest {
	if len(ds) == 0 {
		return model.Dest{Kind: "unknown"}
	}
	known := map[string]model.Dest{}
	var dynamic []model.Dest
	for _, d := range ds {
		if d.Known() {
			if _, ok := known[d.Value]; !ok {
				known[d.Value] = d
			}
		} else {
			dynamic = append(dynamic, d)
		}
	}
	switch {
	case len(known) == 1 && len(dynamic) == 0:
		for _, d := range known {
			return d
		}
	case len(known) >= 1:
		vals := model.SortedKeys(known)
		d := model.Dest{Value: strings.Join(vals, ","), Kind: "multiple"}
		if len(dynamic) > 0 {
			d.Expr = dynamic[0].Expr
		}
		return d
	}
	return dynamic[0]
}

// assignComponents maps every package directory to a component: the
// deployable unit a call or a server belongs to. A repo with one service is
// one component named after the repo; a repo with several main packages is
// split by the main package directories.
func (s *scanner) assignComponents() {
	for _, r := range s.ws.repos {
		dirOf, isMain := s.componentDirFunc(r)
		used := map[string]bool{}
		for _, c := range s.calls {
			if c.repo == r {
				used[dirOf(c.pd)] = true
			}
		}
		for _, c := range s.clients {
			if c.repo == r {
				used[dirOf(c.pd)] = true
			}
		}
		for _, sv := range s.servers {
			if sv.repo == r {
				used[dirOf(sv.pd)] = true
			}
		}
		single := len(used) <= 1
		for _, pd := range r.pkgDirs {
			d := dirOf(pd)
			name := r.name
			if !single && d != r.dir {
				rel, err := filepath.Rel(r.dir, d)
				if err == nil {
					name = r.name + "/" + filepath.ToSlash(rel)
				}
			}
			s.compName[pd.dir] = name
			if _, ok := s.compDir[name]; !ok {
				s.compDir[name] = d
				s.compRepo[name] = r
			}
			if r.cfg.SingleComponent || isMain[d] {
				s.compKind[name] = "service"
			} else if s.compKind[name] == "" {
				s.compKind[name] = "library"
			}
		}
	}
}

func (s *scanner) componentDirFunc(r *repo) (func(*pkgDir) string, map[string]bool) {
	mainsByMod := map[*module][]string{}
	isMain := map[string]bool{}
	for _, pd := range r.pkgDirs {
		if p := s.l.pkgs[pd.dir]; p != nil && p.name == "main" && !p.gen.kitex {
			mainsByMod[pd.mod] = append(mainsByMod[pd.mod], pd.dir)
			isMain[pd.dir] = true
		}
	}
	var serverDirs []string
	for _, sv := range s.servers {
		if sv.repo == r {
			serverDirs = append(serverDirs, sv.pd.dir)
		}
	}
	return func(pd *pkgDir) string {
		if r.cfg.SingleComponent {
			return r.dir
		}
		mains := mainsByMod[pd.mod]
		best := ""
		for _, m := range mains {
			if within(pd.dir, m) && len(m) > len(best) {
				best = m
			}
		}
		if best != "" {
			return best
		}
		var serverMains []string
		for _, m := range mains {
			for _, sd := range serverDirs {
				if within(sd, m) {
					serverMains = append(serverMains, m)
					break
				}
			}
		}
		if len(serverMains) == 1 {
			return serverMains[0]
		}
		if len(mains) == 1 {
			return mains[0]
		}
		return pd.mod.dir
	}, isMain
}

func (s *scanner) comp(pd *pkgDir) string {
	if n, ok := s.compName[pd.dir]; ok {
		return n
	}
	return pd.repo.name
}

func pos(file string, line int) string { return fmt.Sprintf("%s:%d", file, line) }

func (s *scanner) typeName(named *types.Named) string {
	if named == nil {
		return ""
	}
	if p := s.l.byTypes[named.Obj().Pkg()]; p != nil {
		return relPkgPath(p) + "." + named.Obj().Name()
	}
	return named.Obj().Name()
}

// build turns the collected facts into the model.
func (s *scanner) build() *model.Graph {
	g := &model.Graph{
		SchemaVersion: model.SchemaVersion,
		Tool:          "kitex-depgraph",
		Workspace:     s.cfg.Name,
		ScannedAt:     time.Now().Format(time.RFC3339),
	}

	// Repositories.
	for _, r := range s.ws.repos {
		mr := model.Repo{Name: r.name, Path: r.cfg.Path}
		for _, m := range r.modules {
			mr.Modules = append(mr.Modules, m.path)
		}
		comps := map[string]bool{}
		for _, pd := range r.pkgDirs {
			mr.Packages++
			mr.Files += len(pd.files)
			if p := s.l.pkgs[pd.dir]; p != nil {
				if !p.gen.kitex {
					mr.TypeErrors += p.errCount
				}
			}
			comps[s.comp(pd)] = true
		}
		mr.Components = model.SortedKeys(comps)
		g.Repos = append(g.Repos, mr)
		g.Stats.Packages += mr.Packages
		g.Stats.Files += mr.Files
		g.Stats.TypeErrors += mr.TypeErrors
	}
	g.Stats.Repos = len(g.Repos)
	for _, name := range model.SortedKeys(s.compKind) {
		r := s.compRepo[name]
		dir, err := filepath.Rel(r.dir, s.compDir[name])
		if err != nil {
			dir = s.compDir[name]
		}
		g.Components = append(g.Components, model.Component{Name: name, Repo: r.name, Dir: filepath.ToSlash(dir), Kind: s.compKind[name]})
	}

	// Generated service packages.
	var svcPkgs []*lpkg
	for _, p := range s.l.pkgs {
		if p.svc != nil {
			svcPkgs = append(svcPkgs, p)
		}
		if p.pd == nil && p.state == stateDone {
			g.Stats.ExternalLoaded++
		}
	}
	g.Stats.FakedImports = len(s.l.fakes)
	sort.Slice(svcPkgs, func(i, j int) bool {
		a, b := svcPkgs[i], svcPkgs[j]
		if a.svc.key != b.svc.key {
			return a.svc.key < b.svc.key
		}
		return a.dir < b.dir
	})
	svcID := map[*lpkg]string{}
	for i, p := range svcPkgs {
		id := fmt.Sprintf("svc%d", i+1)
		svcID[p] = id
		ms := model.Service{
			ID: id, Key: p.svc.key, PackageName: p.svc.packageName, ServiceName: p.svc.serviceName,
			ImportPath: p.importPath, Source: p.source, KitexVersion: p.svc.kitexVersion,
			Codec: p.svc.codec, IDLFile: p.svc.idlFile, Methods: p.svc.methods,
		}
		if p.pd != nil {
			ms.Repo = p.pd.repo.name
			rel, _ := filepath.Rel(p.pd.repo.dir, p.dir)
			ms.Location = filepath.ToSlash(rel)
		} else {
			ms.Location = p.dir
		}
		g.Services = append(g.Services, ms)
	}

	// Servers.
	for i, sf := range s.servers {
		comp := s.comp(sf.pd)
		ms := model.Server{
			ID: fmt.Sprintf("srv%d", i+1), Repo: sf.repo.name, Component: comp,
			Key: sf.svc.svc.key, Service: svcID[sf.svc], Handler: s.typeName(sf.handler),
			HandlerVia: sf.via, Pos: pos(sf.file, sf.line), Methods: sf.methods,
		}
		if ms.Handler == "" {
			ms.Handler = sf.handlerExpr
		}
		seen := map[string]bool{}
		for _, n := range sf.repo.cfg.ServiceNames {
			if !seen[n] {
				seen[n] = true
				ms.ServerNames = append(ms.ServerNames, model.Dest{Value: n, Kind: "config"})
			}
		}
		for _, nf := range s.names {
			if nf.repo != sf.repo || s.comp(nf.pd) != comp {
				continue
			}
			k := nf.dest.Value + "|" + nf.dest.Expr
			if !seen[k] && !seen[nf.dest.Value] {
				seen[k] = true
				ms.ServerNames = append(ms.ServerNames, nf.dest)
			}
		}
		g.Servers = append(g.Servers, ms)
		if sf.via == "unknown" {
			detail := fmt.Sprintf("handler 表达式 `%s`", sf.handlerExpr)
			if sf.cands > 1 {
				detail += fmt.Sprintf(",仓库内有 %d 个类型都实现了该服务的全部方法", sf.cands)
			}
			g.Issues = append(g.Issues, model.Issue{
				Kind: "handler-unresolved", Severity: "medium", Repo: sf.repo.name, Pos: ms.Pos, Key: ms.Key,
				Title:  fmt.Sprintf("%s 注册的 %s 找不到具体实现类型，无法定位各方法的实现函数", comp, ms.Key),
				Detail: detail,
			})
		}
	}

	// Client creations.
	type compKey struct{ comp, key string }
	clientDests := map[compKey][]model.Dest{}
	for _, cf := range s.clients {
		comp := s.comp(cf.pd)
		g.Clients = append(g.Clients, model.Client{
			Repo: cf.repo.name, Component: comp, Key: cf.svc.svc.key, Service: svcID[cf.svc],
			Dest: cf.dest, Holder: cf.holder, Pos: pos(cf.file, cf.line),
		})
		k := compKey{comp, cf.svc.svc.key}
		clientDests[k] = append(clientDests[k], cf.dest)
	}

	// RPC call sites. When the client could not be traced to its NewClient
	// call, fall back to the component's clients for the same service.
	for _, c := range s.calls {
		comp := s.comp(c.pd)
		d := c.dest
		if d.Kind == "unknown" {
			if ds := clientDests[compKey{comp, c.svc.svc.key}]; len(ds) > 0 {
				d = combineDests(ds)
				if d.Kind == "const" || d.Kind == "var" {
					d.Kind = "inferred"
				}
			}
		}
		g.Calls = append(g.Calls, model.Call{
			ID: c.id, Repo: c.repo.name, Component: comp, Func: c.fn, Pos: pos(c.file, c.line),
			Key: c.svc.svc.key, Service: svcID[c.svc], Method: c.method, Wire: c.wire, Dest: d,
		})
	}

	// Call graph.
	ids := model.SortedKeys(s.funcs)
	for _, id := range ids {
		f := s.funcs[id]
		g.Funcs = append(g.Funcs, model.Func{
			ID: f.id, Repo: f.repo.name, Component: s.comp(f.pd), Name: f.name,
			File: f.file, Line: f.line, EndLine: f.end,
		})
	}
	g.Edges = s.edges
	sort.SliceStable(g.Edges, func(i, j int) bool { return g.Edges[i].From < g.Edges[j].From })
	g.Stats.Funcs = len(g.Funcs)
	g.Stats.Edges = len(g.Edges)

	if s.chaCapped > 0 {
		g.Issues = append(g.Issues, model.Issue{
			Kind: "cha-capped", Severity: "info",
			Title: fmt.Sprintf("%d 处接口调用的候选实现超过 %d 个，调用图中没有展开", s.chaCapped, chaLimit),
		})
	}
	return g
}
