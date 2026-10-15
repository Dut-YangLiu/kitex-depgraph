package query

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Dut-YangLiu/kitex-depgraph/internal/model"
)

// Change is the set of changed lines per file, as found in a unified diff.
type Change struct {
	Lines   map[string][]int // path -> changed line numbers on the new side
	Deleted []string         // files removed entirely
}

var hunkRe = regexp.MustCompile(`^@@ -\d+(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// ParseDiff reads a unified diff (git diff with any -U context, or diff -u)
// and records the new-side lines that were added or modified, and the
// positions of deletions.
func ParseDiff(r io.Reader) (*Change, error) {
	ch := &Change{Lines: map[string][]int{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	oldPath, cur := "", ""
	line, oldLeft, newLeft := 0, 0, 0
	for sc.Scan() {
		s := sc.Text()
		if oldLeft > 0 || newLeft > 0 { // inside a hunk
			switch {
			case strings.HasPrefix(s, "+"):
				if cur != "" {
					ch.Lines[cur] = append(ch.Lines[cur], line)
				}
				line++
				newLeft--
			case strings.HasPrefix(s, "-"):
				if cur != "" {
					ch.Lines[cur] = append(ch.Lines[cur], line) // deletion point
				}
				oldLeft--
			case strings.HasPrefix(s, `\`): // "\ No newline at end of file"
			default:
				line++
				oldLeft--
				newLeft--
			}
			continue
		}
		switch {
		case strings.HasPrefix(s, "--- "):
			oldPath = diffPath(s[4:])
		case strings.HasPrefix(s, "+++ "):
			cur = diffPath(s[4:])
			if cur == "" && oldPath != "" {
				ch.Deleted = append(ch.Deleted, oldPath)
			}
		case strings.HasPrefix(s, "@@"):
			m := hunkRe.FindStringSubmatch(s)
			if m == nil {
				continue
			}
			oldLeft, newLeft = atoiDefault(m[1], 1), atoiDefault(m[3], 1)
			line, _ = strconv.Atoi(m[2])
			if newLeft == 0 && line == 0 {
				line = 1
			}
		}
	}
	return ch, sc.Err()
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func diffPath(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	if s == "/dev/null" {
		return ""
	}
	if strings.HasPrefix(s, "a/") || strings.HasPrefix(s, "b/") {
		s = s[2:]
	}
	return s
}

// ImpactFunc is a function in the impact report.
type ImpactFunc struct {
	Component string `json:"component"`
	Name      string `json:"name"`
	Pos       string `json:"pos"`
}

// AffectedRPC is an RPC method whose implementation reaches changed code.
type AffectedRPC struct {
	RPC     string       `json:"rpc"`
	Server  string       `json:"server"` // implementing component
	Impl    string       `json:"impl"`
	Level   int          `json:"level"` // 1: the changed repo's own RPC; 2+: transitive
	Callers []CallerSite `json:"callers"`
}

// IDLChange is a change inside generated Kitex code.
type IDLChange struct {
	Key          string       `json:"key"`
	Location     string       `json:"location"`
	Implementers []string     `json:"implementers"`
	Callers      []CallerSite `json:"callers"`
}

// ImpactResult answers "what does this change affect, across repositories".
type ImpactResult struct {
	Repo       string        `json:"repo"`
	Changed    []ImpactFunc  `json:"changed"`
	IDL        []IDLChange   `json:"idlChanges,omitempty"`
	RPCs       []AffectedRPC `json:"affectedRPCs"`
	Entries    []ImpactFunc  `json:"entries"`
	Components []string      `json:"components"` // upstream components to involve
	Unmatched  []string      `json:"unmatchedFiles,omitempty"`
}

// ImpactInput says what changed.
type ImpactInput struct {
	Repo    string
	Change  *Change  // from a diff
	Files   []string // whole files
	Funcs   []*model.Func
	MaxHops int // RPC hops upstream; 0 means 10
}

// Impact walks the call graph backwards from the changed functions: to the
// RPC methods whose implementation reaches them, then to every call site of
// those methods in other repositories, and so on upstream.
func Impact(ix *model.Index, in ImpactInput) (*ImpactResult, error) {
	if in.MaxHops <= 0 {
		in.MaxHops = 10
	}
	g := ix.G
	repo := in.Repo
	if repo == "" {
		if len(g.Repos) != 1 {
			return nil, fmt.Errorf("工作区有多个仓库，请用 -repo 指定改动所在的仓库")
		}
		repo = g.Repos[0].Name
	}
	if !knownScope(g, repo) {
		return nil, fmt.Errorf("工作区里没有仓库 %q", repo)
	}
	res := &ImpactResult{Repo: repo}

	changed := map[string]bool{}
	addFunc := func(f *model.Func) {
		if !changed[f.ID] {
			changed[f.ID] = true
			res.Changed = append(res.Changed, ImpactFunc{Component: f.Component, Name: f.Name, Pos: fmt.Sprintf("%s:%d", f.File, f.Line)})
		}
	}
	var paths []string
	if in.Change != nil {
		for p := range in.Change.Lines {
			paths = append(paths, p)
		}
		paths = append(paths, in.Change.Deleted...)
	}
	paths = append(paths, in.Files...)
	sort.Strings(paths)
	for _, p := range paths {
		matched := false
		if idl := idlChange(ix, repo, p); idl != nil {
			res.IDL = appendIDL(res.IDL, idl)
			matched = true
		}
		var lines []int
		whole := containsStr(in.Files, p)
		if in.Change != nil {
			lines = in.Change.Lines[p]
			if containsStr(in.Change.Deleted, p) {
				whole = true
			}
		}
		for i := range g.Funcs {
			f := &g.Funcs[i]
			if f.Repo != repo || !samePath(f.File, p) {
				continue
			}
			if whole || touches(f, lines) {
				addFunc(f)
				matched = true
			}
		}
		if !matched {
			res.Unmatched = append(res.Unmatched, p)
		}
	}
	for _, f := range in.Funcs {
		addFunc(f)
	}

	// Calls grouped by the server and method they reach. Calls in unused
	// library code are left out.
	callsTo := map[string][]*model.Call{}
	for i := range g.Calls {
		c := &g.Calls[i]
		if len(c.UsedBy) == 0 {
			continue
		}
		if l := ix.Link[c.ID]; l != nil {
			for _, sid := range l.Servers {
				callsTo[sid+"/"+c.Wire] = append(callsTo[sid+"/"+c.Wire], c)
			}
		}
	}

	type item struct {
		f     string
		level int
	}
	kinds := componentKinds(g)
	visited := map[string]bool{}
	var queue []item
	for id := range changed {
		visited[id] = true
		queue = append(queue, item{id, 1})
	}
	sort.Slice(queue, func(i, j int) bool { return queue[i].f < queue[j].f })
	affected := map[string]*AffectedRPC{}
	entries := map[string]bool{}
	comps := map[string]bool{}
	for len(queue) > 0 && len(visited) < 200000 {
		it := queue[0]
		queue = queue[1:]
		f := ix.Func[it.f]
		if f != nil && !changed[it.f] && kinds[f.Component] == "service" {
			comps[f.Component] = true
		}
		for _, s := range ix.ServerOf[it.f] {
			for wire, impl := range s.Methods {
				if impl != it.f {
					continue
				}
				k := s.ID + "/" + wire
				ar := affected[k]
				if ar == nil {
					ar = &AffectedRPC{RPC: s.Key + "/" + wire, Server: s.Component, Impl: ix.FuncName(impl), Level: it.level}
					affected[k] = ar
				}
				for _, c := range callsTo[k] {
					for _, comp := range c.UsedBy {
						ar.Callers = append(ar.Callers, CallerSite{Component: comp, Repo: c.Repo,
							Func: ix.FuncName(c.Func), Pos: c.Pos, RPC: c.RPC(), Dest: c.Dest})
						comps[comp] = true
					}
					if it.level < in.MaxHops && !visited[c.Func] {
						visited[c.Func] = true
						queue = append(queue, item{c.Func, it.level + 1})
					}
				}
			}
		}
		ins := ix.In[it.f]
		entry := f != nil && !changed[it.f] && kinds[f.Component] == "service" && !isBoilerplate(f)
		if entry && len(ins) == 0 && len(ix.ServerOf[it.f]) == 0 {
			entries[it.f] = true
		}
		for _, e := range ins {
			if e.Kind == "ref" && entry {
				entries[it.f] = true // registered as a handler or callback
			}
			if !visited[e.From] {
				visited[e.From] = true
				queue = append(queue, item{e.From, it.level})
			}
		}
	}

	for _, k := range model.SortedKeys(affected) {
		res.RPCs = append(res.RPCs, *affected[k])
	}
	sort.SliceStable(res.RPCs, func(i, j int) bool { return res.RPCs[i].Level < res.RPCs[j].Level })
	for _, id := range model.SortedKeys(entries) {
		if f := ix.Func[id]; f != nil {
			res.Entries = append(res.Entries, ImpactFunc{Component: f.Component, Name: f.Name, Pos: fmt.Sprintf("%s:%d", f.File, f.Line)})
		}
	}
	for _, idl := range res.IDL {
		for _, c := range idl.Callers {
			comps[c.Component] = true
		}
		for _, c := range idl.Implementers {
			comps[c] = true
		}
	}
	res.Components = model.SortedKeys(comps)
	return res, nil
}

func componentKinds(g *model.Graph) map[string]string {
	k := map[string]string{}
	for _, c := range g.Components {
		k[c.Name] = c.Kind
	}
	return k
}

// isBoilerplate filters functions that are entries of everything: main,
// init and package-level initializers.
func isBoilerplate(f *model.Func) bool {
	return strings.HasSuffix(f.Name, ".main") || strings.Contains(f.Name, ".init#") ||
		strings.HasSuffix(f.Name, ".<包级变量初始化>")
}

func samePath(funcFile, diffFile string) bool {
	return funcFile == diffFile || strings.HasSuffix(diffFile, "/"+funcFile) || strings.HasSuffix(funcFile, "/"+diffFile)
}

func touches(f *model.Func, lines []int) bool {
	for _, l := range lines {
		if l >= f.Line && l <= f.EndLine {
			return true
		}
	}
	return false
}

// idlChange reports a change in a generated Kitex service package.
func idlChange(ix *model.Index, repo, path string) *IDLChange {
	for _, s := range ix.G.Services {
		if s.Repo != repo || s.Location == "" {
			continue
		}
		if !strings.HasPrefix(path, s.Location+"/") && !strings.Contains(path, "/"+s.Location+"/") {
			continue
		}
		idl := &IDLChange{Key: s.Key, Location: s.Repo + "/" + s.Location}
		for _, sv := range ix.G.Servers {
			if sv.Key == s.Key {
				idl.Implementers = append(idl.Implementers, sv.Component)
			}
		}
		for _, c := range ix.G.Calls {
			if c.Key != s.Key {
				continue
			}
			for _, comp := range c.UsedBy {
				idl.Callers = append(idl.Callers, CallerSite{Component: comp, Repo: c.Repo, Func: ix.FuncName(c.Func), Pos: c.Pos, RPC: c.RPC(), Dest: c.Dest})
			}
		}
		return idl
	}
	return nil
}

func appendIDL(list []IDLChange, x *IDLChange) []IDLChange {
	for _, y := range list {
		if y.Key == x.Key && y.Location == x.Location {
			return list
		}
	}
	return append(list, *x)
}

// Text renders the impact report.
func (r *ImpactResult) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "改动所在仓库:%s\n\n", r.Repo)
	fmt.Fprintf(&b, "改动的函数(%d):\n", len(r.Changed))
	if len(r.Changed) == 0 {
		b.WriteString("  没有落在已知函数里的改动(可能是非 Go 文件、测试文件或生成代码)\n")
	}
	for _, f := range r.Changed {
		fmt.Fprintf(&b, "  %s  %s\n", f.Name, f.Pos)
	}
	for _, idl := range r.IDL {
		fmt.Fprintf(&b, "\nIDL 生成代码有改动:%s(%s)\n", idl.Key, idl.Location)
		fmt.Fprintf(&b, "  实现方:%s\n", orNone(idl.Implementers))
		fmt.Fprintf(&b, "  调用点 %d 处，涉及组件:%s\n", len(idl.Callers), orNone(callerComps(idl.Callers)))
		b.WriteString("  请确认各调用方和实现方重新生成代码、兼容新旧字段后再发布。\n")
	}
	fmt.Fprintf(&b, "\n受影响的 RPC 接口(%d):\n", len(r.RPCs))
	if len(r.RPCs) == 0 {
		b.WriteString("  改动没有被任何 RPC 接口的实现调用到\n")
	}
	for _, a := range r.RPCs {
		tag := ""
		if a.Level > 1 {
			tag = fmt.Sprintf("(传递影响，第 %d 层)", a.Level)
		}
		fmt.Fprintf(&b, "  %s%s  实现:[%s] %s\n", a.RPC, tag, a.Server, a.Impl)
		if len(a.Callers) == 0 {
			b.WriteString("    工作区内没有调用方\n")
		}
		for _, c := range a.Callers {
			fmt.Fprintf(&b, "    ← [%s] %s  %s\n", c.Component, c.Func, c.Where())
		}
	}
	if len(r.Entries) > 0 {
		fmt.Fprintf(&b, "\n受影响的入口(路由/回调处理函数，或没有调用方的函数，%d):\n", len(r.Entries))
		for i, f := range r.Entries {
			if i >= 30 {
				fmt.Fprintf(&b, "  …… 共 %d 个\n", len(r.Entries))
				break
			}
			fmt.Fprintf(&b, "  [%s] %s  %s\n", f.Component, f.Name, f.Pos)
		}
	}
	fmt.Fprintf(&b, "\n需要知会/回归的组件:%s\n", orNone(r.Components))
	if len(r.Unmatched) > 0 {
		fmt.Fprintf(&b, "未匹配到函数的文件:%s\n", strings.Join(r.Unmatched, "、"))
	}
	return b.String()
}

func callerComps(cs []CallerSite) []string {
	m := map[string]bool{}
	for _, c := range cs {
		m[c.Component] = true
	}
	return model.SortedKeys(m)
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "无"
	}
	return strings.Join(xs, "、")
}
