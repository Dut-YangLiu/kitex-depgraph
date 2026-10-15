package scan

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// The loader type-checks workspace packages from source with go/types, but
// without a build environment: no `go list`, no downloaded dependencies.
//
//   - workspace packages are always loaded from source;
//   - Kitex generated packages (kitex_gen) and first-party modules are loaded
//     from source even when they live in vendor/ or the module cache;
//   - every other import (standard library, frameworks, third-party code) is
//     replaced by an empty stand-in package.
//
// Expressions that depend on a stand-in package simply get an invalid type.
// That is harmless here: everything the analysis needs (Kitex Client types,
// workspace functions, struct fields, constants) is declared in packages that
// are loaded from source.

const (
	stateNew = iota
	stateLoading
	stateDone
	stateSkipped
)

type lpkg struct {
	dir        string
	importPath string
	pd         *pkgDir // non-nil for workspace packages
	mod        *module // module used to resolve this package's imports
	source     string  // workspace | vendor | modcache | local
	name       string
	files      []*ast.File
	types      *types.Package
	info       *types.Info
	gen        genHeader
	svc        *svcInfo // non-nil for Kitex generated service packages
	errCount   int
	errSample  []string
	state      int
}

type loader struct {
	ws       *workspace
	cfg      *Config
	fset     *token.FileSet
	modcache string
	bctx     build.Context
	sizes    types.Sizes
	pkgs     map[string]*lpkg // by absolute dir
	byTypes  map[*types.Package]*lpkg
	fakes    map[string]*types.Package
	// missingGen records imports of generated Kitex code whose source could
	// not be found: RPC calls through them cannot be recognised.
	missingGen map[string][]string // import path -> importing repos
	onLoaded   func(*lpkg)
	log        io.Writer
	verbose    bool
}

func newLoader(ws *workspace, cfg *Config) *loader {
	return &loader{
		ws:         ws,
		cfg:        cfg,
		fset:       token.NewFileSet(),
		modcache:   findModCache(),
		bctx:       build.Default,
		sizes:      types.SizesFor("gc", runtime.GOARCH),
		pkgs:       map[string]*lpkg{},
		byTypes:    map[*types.Package]*lpkg{},
		fakes:      map[string]*types.Package{},
		missingGen: map[string][]string{},
		log:        io.Discard,
	}
}

// Import implements types.Importer.
func (l *loader) Import(path string) (*types.Package, error) { return l.ImportFrom(path, "", 0) }

// ImportFrom implements types.ImporterFrom. srcDir is the directory of the
// importing file.
func (l *loader) ImportFrom(path, srcDir string, _ types.ImportMode) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	from := l.pkgs[srcDir]
	if dir, source, mod := l.resolve(path, from); dir != "" {
		if p := l.load(dir, path, source, mod); p.state == stateDone && p.types != nil {
			return p.types, nil
		}
	} else if l.isGenPath(path) && from != nil && from.pd != nil {
		repos := l.missingGen[path]
		if len(repos) == 0 || repos[len(repos)-1] != from.pd.repo.name {
			l.missingGen[path] = append(repos, from.pd.repo.name)
		}
	}
	return l.fake(path), nil
}

// fake returns an empty, complete stand-in package.
func (l *loader) fake(path string) *types.Package {
	if p, ok := l.fakes[path]; ok {
		return p
	}
	p := types.NewPackage(path, guessPkgName(path))
	p.MarkComplete()
	l.fakes[path] = p
	return p
}

var majorVersion = regexp.MustCompile(`^v[0-9]+$`)

// guessPkgName guesses the package name of an import path that is not loaded.
func guessPkgName(path string) string {
	parts := strings.Split(path, "/")
	name := parts[len(parts)-1]
	if majorVersion.MatchString(name) && len(parts) > 1 {
		name = parts[len(parts)-2]
	}
	if i := strings.Index(name, ".v"); i > 0 { // gopkg.in/yaml.v3
		name = name[:i]
	}
	name = strings.TrimPrefix(name, "go-")
	name = strings.Map(func(r rune) rune {
		if r == '-' || r == '.' {
			return '_'
		}
		return r
	}, name)
	if name == "" {
		name = "_"
	}
	return name
}

// resolve maps an import path to a directory, using the importing package's
// module for vendor, replace and require information.
func (l *loader) resolve(path string, from *lpkg) (dir, source string, mod *module) {
	var ctx *module
	var fromRepo *repo
	fromDir := ""
	if from != nil {
		ctx = from.mod
		fromDir = from.dir
		if from.pd != nil {
			fromRepo = from.pd.repo
		}
	}
	first := path
	if i := strings.IndexByte(path, '/'); i >= 0 {
		first = path[:i]
	}
	hasDot := strings.Contains(first, ".")

	if ctx != nil && ctx.vendor && hasDot {
		if d := filepath.Join(ctx.dir, "vendor", filepath.FromSlash(path)); isDir(d) {
			return d, "vendor", ctx
		}
	}
	if ctx != nil {
		for _, r := range ctx.replaces {
			if r.localDir == "" {
				continue
			}
			if rest, ok := cutModPrefix(path, r.OldPath); ok {
				d := filepath.Join(r.localDir, filepath.FromSlash(rest))
				if pd := l.ws.pkgDirs[d]; pd != nil {
					return d, "workspace", pd.mod
				}
				if isDir(d) {
					return d, "local", ctx
				}
			}
		}
	}
	if cands := l.ws.byPath[path]; len(cands) > 0 {
		best := pickCandidate(cands, ctx, fromRepo, fromDir)
		return best.dir, "workspace", best.mod
	}
	if !hasDot || ctx == nil || l.modcache == "" {
		return "", "", nil // standard library or unknown: use a stand-in
	}
	if modPath, ver, ok := ctx.requiredModule(path); ok {
		rest, _ := cutModPrefix(path, modPath)
		newPath, newVer := modPath, ver
		for _, r := range ctx.replaces {
			if r.localDir == "" && r.OldPath == modPath && (r.OldVersion == "" || r.OldVersion == ver) && r.NewVersion != "" {
				newPath, newVer = r.NewPath, r.NewVersion
			}
		}
		d := filepath.Join(l.modcache, escapeModPath(newPath)+"@"+escapeModPath(newVer), filepath.FromSlash(rest))
		if isDir(d) {
			return d, "modcache", ctx
		}
	}
	return "", "", nil
}

// pickCandidate chooses among workspace packages that share an import path:
// same module, then same repo, then the closest directory.
func pickCandidate(cands []*pkgDir, ctx *module, fromRepo *repo, fromDir string) *pkgDir {
	if len(cands) == 1 {
		return cands[0]
	}
	best, bestScore := cands[0], -1
	for _, c := range cands {
		score := commonPrefixLen(c.dir, fromDir)
		if fromRepo != nil && c.repo == fromRepo {
			score += 1 << 20
		}
		if ctx != nil && c.mod == ctx {
			score += 1 << 21
		}
		if score > bestScore {
			best, bestScore = c, score
		}
	}
	return best
}

func commonPrefixLen(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// load parses and type-checks the package in dir (memoized by directory).
func (l *loader) load(dir, importPath, source string, mod *module) *lpkg {
	if p, ok := l.pkgs[dir]; ok {
		return p
	}
	p := &lpkg{dir: dir, importPath: importPath, source: source, mod: mod, pd: l.ws.pkgDirs[dir]}
	l.pkgs[dir] = p
	var fileNames []string
	if p.pd != nil {
		fileNames = p.pd.files
		p.source = "workspace"
		p.mod = p.pd.mod
	} else {
		fileNames = l.listGoFiles(dir)
		if !l.allowExternal(importPath, fileNames) {
			p.state = stateSkipped
			return p
		}
	}
	p.state = stateLoading

	var files []*ast.File
	for _, fn := range fileNames {
		// AllErrors stops the parser from bailing out after 10 errors, so a
		// file with a syntax error still yields the declarations before it.
		f, err := parser.ParseFile(l.fset, fn, nil, parser.ParseComments|parser.SkipObjectResolution|parser.AllErrors)
		if f == nil || f.Name == nil || f.Name.Name == "" {
			p.errCount++
			p.addErr(err)
			continue
		}
		if err != nil {
			p.errCount++
			p.addErr(err)
		}
		files = append(files, f)
	}
	files, p.name = majorityPackage(files)
	p.files = files
	p.gen = detectGen(files, importPath, l.cfg.GenDirNames)

	needInfo := p.pd != nil && !p.gen.kitex
	conf := types.Config{
		Importer:                 l,
		FakeImportC:              true,
		IgnoreFuncBodies:         !needInfo,
		DisableUnusedImportCheck: true,
		Sizes:                    l.sizes,
		Error: func(err error) {
			p.errCount++
			p.addErr(err)
		},
	}
	if needInfo {
		p.info = &types.Info{
			Types:      map[ast.Expr]types.TypeAndValue{},
			Defs:       map[*ast.Ident]types.Object{},
			Uses:       map[*ast.Ident]types.Object{},
			Selections: map[*ast.SelectorExpr]*types.Selection{},
		}
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				p.errCount++
				p.addErr(fmt.Errorf("类型检查异常: %v", r))
			}
		}()
		p.types, _ = conf.Check(importPath, l.fset, files, p.info)
	}()
	if p.types == nil {
		p.types = types.NewPackage(importPath, p.name)
		p.types.MarkComplete()
	}
	p.state = stateDone
	l.byTypes[p.types] = p
	if p.gen.kitex {
		p.svc = extractService(p)
	}
	if l.onLoaded != nil {
		l.onLoaded(p)
	}
	return p
}

func (p *lpkg) addErr(err error) {
	if err != nil && len(p.errSample) < 5 {
		p.errSample = append(p.errSample, err.Error())
	}
}

// majorityPackage drops files whose package clause disagrees with the
// majority (e.g. stray "package main" scripts without build tags).
func majorityPackage(files []*ast.File) ([]*ast.File, string) {
	count := map[string]int{}
	for _, f := range files {
		if f.Name.Name != "" {
			count[f.Name.Name]++
		}
	}
	best := ""
	for name, n := range count {
		if n > count[best] || (n == count[best] && name < best) {
			best = name
		}
	}
	if len(count) <= 1 {
		return files, best
	}
	out := files[:0:0]
	for _, f := range files {
		if f.Name.Name == best {
			out = append(out, f)
		}
	}
	return out, best
}

func (l *loader) listGoFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if ok, err := l.bctx.MatchFile(dir, name); err == nil && ok {
			out = append(out, filepath.Join(dir, name))
		}
	}
	sort.Strings(out)
	return out
}

func (l *loader) isGenPath(importPath string) bool {
	for _, seg := range strings.Split(importPath, "/") {
		for _, g := range l.cfg.GenDirNames {
			if seg == g {
				return true
			}
		}
	}
	return false
}

// allowExternal decides whether a package outside the workspace is loaded
// from source instead of being replaced by a stand-in.
func (l *loader) allowExternal(importPath string, files []string) bool {
	if len(files) == 0 {
		return false
	}
	if l.isGenPath(importPath) {
		return true
	}
	for _, p := range l.cfg.FirstPartyPrefixes {
		if strings.HasPrefix(importPath, p) {
			return true
		}
	}
	for i, fn := range files {
		if i >= 8 {
			break
		}
		if g := sniffHeader(fn); g == genKitex || g == genThriftgo {
			return true
		}
	}
	return false
}

func sniffHeader(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 1024)
	n, _ := io.ReadFull(f, buf)
	kind, _ := classifyHeader(string(buf[:n]))
	return kind
}
