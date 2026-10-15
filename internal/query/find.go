// Package query answers questions on top of a linked graph: who calls an
// RPC, which repositories a feature touches, what a change affects.
package query

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Dut-YangLiu/kitex-depgraph/internal/model"
)

// FindFuncs resolves a user pattern to functions.
//
//	CreateOrder                 any function whose name contains the text
//	order-svc:CreateOrder       restricted to a repo or component
//	rpc:order.OrderService/CreateOrder   the implementation(s) of an RPC method
func FindFuncs(ix *model.Index, pattern string) ([]*model.Func, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, fmt.Errorf("请指定函数名")
	}
	if strings.HasPrefix(pattern, "rpc:") {
		return rpcImpls(ix, strings.TrimPrefix(pattern, "rpc:"))
	}
	scope := ""
	if i := strings.Index(pattern, ":"); i > 0 && knownScope(ix.G, pattern[:i]) {
		scope, pattern = pattern[:i], pattern[i+1:]
	}
	best, bestScore := []*model.Func(nil), 0
	for i := range ix.G.Funcs {
		f := &ix.G.Funcs[i]
		if scope != "" && f.Repo != scope && f.Component != scope {
			continue
		}
		score := matchScore(f.Name, pattern)
		if score == 0 {
			continue
		}
		if score > bestScore {
			best, bestScore = nil, score
		}
		if score == bestScore {
			best = append(best, f)
		}
	}
	if len(best) == 0 {
		return nil, fmt.Errorf("没有找到匹配 %q 的函数(可用 \"仓库名:函数名\" 缩小范围，或 rpc:服务/方法 从 RPC 实现开始)", pattern)
	}
	// Prefer service code over shared library code with the same name.
	kinds := componentKinds(ix.G)
	var services []*model.Func
	for _, f := range best {
		if kinds[f.Component] != "library" {
			services = append(services, f)
		}
	}
	if len(services) > 0 {
		best = services
	}
	sort.Slice(best, func(i, j int) bool { return best[i].ID < best[j].ID })
	return best, nil
}

func knownScope(g *model.Graph, s string) bool {
	for _, r := range g.Repos {
		if r.Name == s {
			return true
		}
	}
	for _, c := range g.Components {
		if c.Name == s {
			return true
		}
	}
	return false
}

// matchScore ranks a function name against a pattern: exact name, then the
// function/method name itself, then a suffix, then a substring.
func matchScore(name, pattern string) int {
	switch {
	case name == pattern:
		return 4
	case lastSegment(name) == pattern:
		return 3
	case strings.HasSuffix(name, pattern):
		return 2
	case strings.Contains(name, pattern):
		return 1
	}
	return 0
}

func lastSegment(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

func rpcImpls(ix *model.Index, spec string) ([]*model.Func, error) {
	key, method := splitRPC(spec)
	var out []*model.Func
	for _, s := range ix.G.Servers {
		if s.Key != key {
			continue
		}
		for wire, fid := range s.Methods {
			if method != "" && !strings.EqualFold(wire, method) {
				continue
			}
			if f := ix.Func[fid]; f != nil {
				out = append(out, f)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("工作区里没有找到 %s 的实现", spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func splitRPC(spec string) (key, method string) {
	if i := strings.Index(spec, "/"); i >= 0 {
		return spec[:i], spec[i+1:]
	}
	return spec, ""
}

// CallerSite is one call site of an RPC.
type CallerSite struct {
	Component string     `json:"component"`
	Repo      string     `json:"repo"`
	Func      string     `json:"func"`
	Pos       string     `json:"pos"`
	RPC       string     `json:"rpc"`
	Dest      model.Dest `json:"dest"`
	Status    string     `json:"status"`
	Servers   []string   `json:"servers,omitempty"` // implementing components
}

// Where is the call position, prefixed with its repository when the call
// lives in another repository than its component (shared library code).
func (c CallerSite) Where() string {
	if c.Repo != "" && c.Component != c.Repo && !strings.HasPrefix(c.Component, c.Repo+"/") {
		return c.Repo + ":" + c.Pos
	}
	return c.Pos
}

// Impl is one implementation of an RPC method.
type Impl struct {
	Component string   `json:"component"`
	Repo      string   `json:"repo"`
	RPC       string   `json:"rpc"`
	Func      string   `json:"func,omitempty"`
	Pos       string   `json:"pos,omitempty"`
	Names     []string `json:"serverNames,omitempty"`
}

// CallersResult answers "who calls this RPC".
type CallersResult struct {
	RPC     string       `json:"rpc"`
	Impls   []Impl       `json:"implementations"`
	Callers []CallerSite `json:"callers"`
}

// Callers lists the implementations and the call sites of an RPC service or
// method ("user.UserService" or "user.UserService/GetUser").
func Callers(ix *model.Index, spec string) (*CallersResult, error) {
	key, method := splitRPC(spec)
	res := &CallersResult{RPC: spec}
	known := false
	for _, s := range ix.G.Services {
		if s.Key == key {
			known = true
		}
	}
	if !known {
		return nil, fmt.Errorf("工作区里没有 IDL 服务 %q(格式：包名.服务名，例如 user.UserService)", key)
	}
	for _, s := range ix.G.Servers {
		if s.Key != key {
			continue
		}
		for _, wire := range model.SortedKeys(s.Methods) {
			if method != "" && !strings.EqualFold(wire, method) {
				continue
			}
			im := Impl{Component: s.Component, Repo: s.Repo, RPC: key + "/" + wire, Names: s.Names()}
			if f := ix.Func[s.Methods[wire]]; f != nil {
				im.Func, im.Pos = f.Name, fmt.Sprintf("%s:%d", f.File, f.Line)
			}
			res.Impls = append(res.Impls, im)
		}
		if len(s.Methods) == 0 {
			res.Impls = append(res.Impls, Impl{Component: s.Component, Repo: s.Repo, RPC: key, Pos: s.Pos, Names: s.Names()})
		}
	}
	for _, c := range ix.G.Calls {
		if c.Key != key || (method != "" && !strings.EqualFold(c.Wire, method) && !strings.EqualFold(c.Method, method)) {
			continue
		}
		site := CallerSite{Repo: c.Repo, Func: ix.FuncName(c.Func), Pos: c.Pos, RPC: c.RPC(), Dest: c.Dest}
		if l := ix.Link[c.ID]; l != nil {
			site.Status = l.Status
			for _, sid := range l.Servers {
				if s := ix.Server[sid]; s != nil {
					site.Servers = append(site.Servers, s.Component)
				}
			}
		}
		comps := c.UsedBy
		if len(comps) == 0 {
			comps = []string{c.Component + "(未被使用的库代码)"}
		}
		for _, comp := range comps {
			s := site
			s.Component = comp
			res.Callers = append(res.Callers, s)
		}
	}
	sort.SliceStable(res.Callers, func(i, j int) bool {
		a, b := res.Callers[i], res.Callers[j]
		if a.Component != b.Component {
			return a.Component < b.Component
		}
		return a.Pos < b.Pos
	})
	return res, nil
}

// Text renders the callers result.
func (r *CallersResult) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n实现方(%d):\n", r.RPC, len(r.Impls))
	if len(r.Impls) == 0 {
		b.WriteString("  工作区里没有实现方\n")
	}
	for _, im := range r.Impls {
		fmt.Fprintf(&b, "  [%s] %s", im.Component, im.RPC)
		if im.Func != "" {
			fmt.Fprintf(&b, "  %s  %s", im.Func, im.Pos)
		}
		if len(im.Names) > 0 {
			fmt.Fprintf(&b, "  注册名=%s", strings.Join(im.Names, ","))
		}
		b.WriteString("\n")
	}
	comps := map[string]bool{}
	for _, c := range r.Callers {
		comps[c.Component] = true
	}
	fmt.Fprintf(&b, "\n调用方(%d 处，来自 %d 个组件):\n", len(r.Callers), len(comps))
	if len(r.Callers) == 0 {
		b.WriteString("  工作区里没有调用方\n")
	}
	last := ""
	for _, c := range r.Callers {
		if c.Component != last {
			fmt.Fprintf(&b, "  %s\n", c.Component)
			last = c.Component
		}
		fmt.Fprintf(&b, "    %s  %s  %s  destService=%s", c.RPC, c.Func, c.Where(), c.Dest.String())
		if c.Status != "ok" && c.Status != "" {
			fmt.Fprintf(&b, "  [%s]", statusText(c.Status))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func statusText(s string) string {
	switch s {
	case "ok":
		return "已匹配"
	case "ambiguous":
		return "多个候选实现方"
	case "external":
		return "工作区外"
	case "missing-method":
		return "实现方缺少该方法"
	}
	return s
}
