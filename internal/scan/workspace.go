package scan

import (
	"encoding/json"
	"fmt"
	"go/build"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Config is the workspace configuration file (JSON).
type Config struct {
	// Name of the workspace, shown in reports.
	Name string `json:"name"`
	// Repos to scan. Paths are relative to the config file.
	Repos []RepoConfig `json:"repos"`
	// GenDirNames are directory names that hold Kitex generated code.
	// Packages below them are always loaded from source, even from the
	// module cache. Default: ["kitex_gen"].
	GenDirNames []string `json:"genDirNames,omitempty"`
	// FirstPartyPrefixes are module path prefixes (e.g. "code.example.com/")
	// whose packages are loaded from the module cache when a workspace
	// package imports them. Everything else outside the workspace is replaced
	// by an empty stand-in package, so no build environment is needed.
	FirstPartyPrefixes []string `json:"firstPartyPrefixes,omitempty"`
	// IncludeTests also scans _test.go files.
	IncludeTests bool `json:"includeTests,omitempty"`
	// SkipDirs are extra directory names to skip while walking repos.
	SkipDirs []string `json:"skipDirs,omitempty"`

	baseDir string
}

// RepoConfig describes one repository.
type RepoConfig struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// ServiceNames are the names this repo's servers register under (the
	// destService callers use). Optional: the scanner also reads them from
	// server.WithServerBasicInfo.
	ServiceNames []string `json:"serviceNames,omitempty"`
	// SingleComponent treats the whole repo as one deployable component even
	// when it contains several main packages.
	SingleComponent bool `json:"singleComponent,omitempty"`

	absPath string
}

// LoadConfig reads a workspace config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("解析配置 %s 失败: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c.baseDir = filepath.Dir(abs)
	if c.Name == "" {
		c.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return &c, c.normalize()
}

// ConfigFromDirs builds a config from plain directories; each directory is
// one repo named after its base name.
func ConfigFromDirs(dirs []string) (*Config, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	c := &Config{Name: "workspace", baseDir: wd}
	for _, d := range dirs {
		abs, err := filepath.Abs(d)
		if err != nil {
			return nil, err
		}
		c.Repos = append(c.Repos, RepoConfig{Name: filepath.Base(abs), Path: d})
	}
	return c, c.normalize()
}

func (c *Config) normalize() error {
	if len(c.Repos) == 0 {
		return fmt.Errorf("没有配置任何仓库")
	}
	if len(c.GenDirNames) == 0 {
		c.GenDirNames = []string{"kitex_gen"}
	}
	seen := map[string]int{}
	for i := range c.Repos {
		r := &c.Repos[i]
		if r.Path == "" {
			return fmt.Errorf("第 %d 个仓库缺少 path", i+1)
		}
		p := r.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(c.baseDir, p)
		}
		p = filepath.Clean(p)
		st, err := os.Stat(p)
		if err != nil || !st.IsDir() {
			return fmt.Errorf("仓库目录不存在: %s", p)
		}
		r.absPath = p
		if r.Name == "" {
			r.Name = filepath.Base(p)
		}
		// Keep names unique: a second "user" becomes "user-2".
		seen[r.Name]++
		if n := seen[r.Name]; n > 1 {
			r.Name = fmt.Sprintf("%s-%d", r.Name, n)
		}
	}
	return nil
}

type workspace struct {
	cfg      *Config
	repos    []*repo
	pkgDirs  map[string]*pkgDir   // by absolute dir
	byPath   map[string][]*pkgDir // by import path
	warnings []string
}

type repo struct {
	name    string
	dir     string
	cfg     RepoConfig
	modules []*module
	pkgDirs []*pkgDir
}

type module struct {
	path     string
	dir      string
	repo     *repo
	gomod    *goMod
	vendor   bool
	replaces []resolvedReplace
}

type resolvedReplace struct {
	replaceDirective
	localDir string // absolute directory for local replacements
}

// requiredModule returns the required module that provides importPath.
func (m *module) requiredModule(importPath string) (string, string, bool) {
	best, ver := "", ""
	for mp, v := range m.gomod.Requires {
		if _, ok := cutModPrefix(importPath, mp); ok && len(mp) > len(best) {
			best, ver = mp, v
		}
	}
	return best, ver, best != ""
}

type pkgDir struct {
	dir        string
	importPath string
	mod        *module
	repo       *repo
	files      []string // absolute paths that match the build constraints
}

// cutModPrefix reports whether importPath is modPath or below it, and returns
// the remainder without a leading slash.
func cutModPrefix(importPath, modPath string) (string, bool) {
	if importPath == modPath {
		return "", true
	}
	if strings.HasPrefix(importPath, modPath+"/") {
		return importPath[len(modPath)+1:], true
	}
	return "", false
}

func (c *Config) skipDir(name string) bool {
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return true
	}
	switch name {
	case "testdata", "vendor", "node_modules":
		return true
	}
	for _, s := range c.SkipDirs {
		if s == name {
			return true
		}
	}
	return false
}

// discover walks every repo, finds Go modules and package directories.
func discover(cfg *Config) (*workspace, error) {
	ws := &workspace{cfg: cfg, pkgDirs: map[string]*pkgDir{}, byPath: map[string][]*pkgDir{}}
	roots := map[string]bool{}
	for _, rc := range cfg.Repos {
		roots[rc.absPath] = true
	}
	bctx := build.Default
	for _, rc := range cfg.Repos {
		r := &repo{name: rc.Name, dir: rc.absPath, cfg: rc}
		ws.repos = append(ws.repos, r)

		modByDir := map[string]*module{}
		goFiles := map[string][]string{}
		err := filepath.WalkDir(r.dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable entries are skipped
			}
			if d.IsDir() {
				if path != r.dir && (cfg.skipDir(d.Name()) || roots[path]) {
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			dir := filepath.Dir(path)
			switch {
			case name == "go.mod":
				data, err := os.ReadFile(path)
				if err != nil {
					return nil
				}
				gm, err := parseGoMod(data)
				if err != nil {
					ws.warnings = append(ws.warnings, fmt.Sprintf("%s: %v", path, err))
					return nil
				}
				m := &module{path: gm.Module, dir: dir, repo: r, gomod: gm}
				if st, err := os.Stat(filepath.Join(dir, "vendor", "modules.txt")); err == nil && !st.IsDir() {
					m.vendor = true
				}
				for _, rp := range gm.Replaces {
					rr := resolvedReplace{replaceDirective: rp}
					if rp.isLocal() {
						ld := rp.NewPath
						if !filepath.IsAbs(ld) {
							ld = filepath.Join(dir, ld)
						}
						rr.localDir = filepath.Clean(ld)
					}
					m.replaces = append(m.replaces, rr)
				}
				modByDir[dir] = m
				r.modules = append(r.modules, m)
			case strings.HasSuffix(name, ".go"):
				if !cfg.IncludeTests && strings.HasSuffix(name, "_test.go") {
					return nil
				}
				if ok, err := bctx.MatchFile(dir, name); err != nil || !ok {
					return nil
				}
				goFiles[dir] = append(goFiles[dir], path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Slice(r.modules, func(i, j int) bool { return r.modules[i].dir < r.modules[j].dir })

		dirs := make([]string, 0, len(goFiles))
		for d := range goFiles {
			dirs = append(dirs, d)
		}
		sort.Strings(dirs)
		orphans := 0
		for _, d := range dirs {
			m := nearestModule(d, r.dir, modByDir)
			if m == nil {
				orphans++
				continue
			}
			rel, _ := filepath.Rel(m.dir, d)
			ip := m.path
			if rel != "." {
				ip = m.path + "/" + filepath.ToSlash(rel)
			}
			files := goFiles[d]
			sort.Strings(files)
			pd := &pkgDir{dir: d, importPath: ip, mod: m, repo: r, files: files}
			r.pkgDirs = append(r.pkgDirs, pd)
			ws.pkgDirs[d] = pd
			ws.byPath[ip] = append(ws.byPath[ip], pd)
		}
		if orphans > 0 {
			ws.warnings = append(ws.warnings, fmt.Sprintf("仓库 %s 有 %d 个目录不属于任何 Go module(没有 go.mod),已跳过", r.name, orphans))
		}
		if len(r.modules) == 0 {
			ws.warnings = append(ws.warnings, fmt.Sprintf("仓库 %s 里没有找到 go.mod", r.name))
		}
	}
	return ws, nil
}

func nearestModule(dir, stop string, modByDir map[string]*module) *module {
	for d := dir; ; d = filepath.Dir(d) {
		if m := modByDir[d]; m != nil {
			return m
		}
		if d == stop || d == filepath.Dir(d) {
			return nil
		}
	}
}

// within reports whether dir is root or below it.
func within(dir, root string) bool {
	if dir == root {
		return true
	}
	rel, err := filepath.Rel(root, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
