package query

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Dut-YangLiu/kitex-depgraph/internal/model"
)

// TraceNode is one node of a trace tree.
type TraceNode struct {
	Kind      string       `json:"kind"` // func | rpc | external | unresolved
	Func      string       `json:"func,omitempty"`
	Name      string       `json:"name"`
	Component string       `json:"component,omitempty"`
	Pos       string       `json:"pos,omitempty"`
	Via       string       `json:"via,omitempty"` // call | iface | ref | rpc
	RPC       string       `json:"rpc,omitempty"`
	Status    string       `json:"status,omitempty"`
	Seen      bool         `json:"seen,omitempty"`      // already expanded elsewhere, or recursion
	Truncated bool         `json:"truncated,omitempty"` // depth limit reached
	Children  []*TraceNode `json:"children,omitempty"`
}

// TraceRPC is one RPC hop found by a trace.
type TraceRPC struct {
	From   string   `json:"from"`
	RPC    string   `json:"rpc"`
	To     []string `json:"to"`
	Status string   `json:"status"`
}

// TraceResult is the answer to "which repositories does this feature touch,
// and through which RPC calls".
type TraceResult struct {
	From       []string     `json:"from"`
	Roots      []*TraceNode `json:"roots"`
	Components []string     `json:"components"`
	RPCs       []TraceRPC   `json:"rpcs"`
	External   []string     `json:"external"`
}

// TraceOptions tune a trace.
type TraceOptions struct {
	MaxDepth int  // function hops; 0 means 40
	Full     bool // keep branches that lead to no RPC call
}

type tracer struct {
	ix       *model.Index
	opt      TraceOptions
	leads    map[string]bool // functions from which some RPC call is reachable
	expanded map[string]bool
	res      *TraceResult
	comps    map[string]bool
	rpcs     map[string]int
}

// Trace expands the cross-repository call tree from the given functions.
func Trace(ix *model.Index, starts []*model.Func, opt TraceOptions) *TraceResult {
	if opt.MaxDepth <= 0 {
		opt.MaxDepth = 40
	}
	t := &tracer{ix: ix, opt: opt, expanded: map[string]bool{}, res: &TraceResult{},
		comps: map[string]bool{}, rpcs: map[string]int{}}
	t.leads = leadsToRPC(ix)
	for _, f := range topLevel(ix, starts) {
		t.res.From = append(t.res.From, f.Name)
		t.res.Roots = append(t.res.Roots, t.build(f.ID, "", 0, map[string]bool{}))
	}
	sort.Strings(t.res.External)
	return t.res
}

// topLevel drops start functions that another start function reaches, so
// "CreateOrder" matching both a handler and the helper it calls yields one
// tree rooted at the handler.
func topLevel(ix *model.Index, starts []*model.Func) []*model.Func {
	if len(starts) < 2 {
		return starts
	}
	isStart := map[string]bool{}
	for _, f := range starts {
		isStart[f.ID] = true
	}
	reached := map[string]bool{}
	for _, f := range starts {
		seen := map[string]bool{f.ID: true}
		queue := []string{f.ID}
		for len(queue) > 0 && len(seen) < 20000 {
			cur := queue[0]
			queue = queue[1:]
			for _, e := range ix.Out[cur] {
				if e.To == "" || seen[e.To] {
					continue
				}
				seen[e.To] = true
				if isStart[e.To] {
					reached[e.To] = true
				}
				queue = append(queue, e.To)
			}
		}
	}
	var out []*model.Func
	for _, f := range starts {
		if !reached[f.ID] {
			out = append(out, f)
		}
	}
	if len(out) == 0 { // mutual recursion: keep them all
		return starts
	}
	return out
}

// leadsToRPC marks every function from which an RPC call site is reachable,
// by a reverse breadth-first search from the functions that make RPC calls.
func leadsToRPC(ix *model.Index) map[string]bool {
	leads := map[string]bool{}
	var queue []string
	for _, c := range ix.G.Calls {
		if !leads[c.Func] {
			leads[c.Func] = true
			queue = append(queue, c.Func)
		}
	}
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		for _, e := range ix.In[f] {
			if !leads[e.From] {
				leads[e.From] = true
				queue = append(queue, e.From)
			}
		}
	}
	return leads
}

func (t *tracer) noteComp(c string) {
	if c != "" && !t.comps[c] {
		t.comps[c] = true
		t.res.Components = append(t.res.Components, c)
	}
}

func (t *tracer) build(fid, via string, depth int, path map[string]bool) *TraceNode {
	n := &TraceNode{Kind: "func", Func: fid, Name: t.ix.FuncName(fid), Via: via}
	if f := t.ix.Func[fid]; f != nil {
		n.Component = f.Component
		n.Pos = fmt.Sprintf("%s:%d", f.File, f.Line)
		t.noteComp(f.Component)
	}
	if path[fid] || t.expanded[fid] {
		n.Seen = true
		return n
	}
	t.expanded[fid] = true
	if depth >= t.opt.MaxDepth {
		n.Truncated = true
		return n
	}
	path[fid] = true
	defer delete(path, fid)

	edges := append([]model.Edge(nil), t.ix.Out[fid]...)
	sort.SliceStable(edges, func(i, j int) bool { return edges[i].Line < edges[j].Line })
	rpcDone := map[string]bool{}
	for _, e := range edges {
		if e.Kind == "rpc" {
			c := t.ix.Call[e.Call]
			if c == nil || rpcDone[c.RPC()] {
				continue
			}
			rpcDone[c.RPC()] = true
			n.Children = append(n.Children, t.rpcNode(c, n.Component, depth, path))
			continue
		}
		if e.To == "" || (!t.opt.Full && !t.leads[e.To]) {
			continue
		}
		n.Children = append(n.Children, t.build(e.To, e.Kind, depth+1, path))
	}
	return n
}

func (t *tracer) rpcNode(c *model.Call, fromComp string, depth int, path map[string]bool) *TraceNode {
	n := &TraceNode{Kind: "rpc", Name: "RPC " + c.RPC(), RPC: c.RPC(), Pos: c.Repo + ":" + c.Pos, Via: "rpc"}
	l := t.ix.Link[c.ID]
	if l == nil {
		return n
	}
	n.Status = l.Status
	var to []string
	for _, sid := range l.Servers {
		if s := t.ix.Server[sid]; s != nil {
			to = append(to, s.Component)
		}
	}
	// Record the hop before descending, so the summary follows call order.
	key := fromComp + "|" + c.RPC()
	if _, ok := t.rpcs[key]; !ok {
		t.rpcs[key] = len(t.res.RPCs)
		t.res.RPCs = append(t.res.RPCs, TraceRPC{From: fromComp, RPC: c.RPC(), To: to, Status: l.Status})
	}
	if l.Status == "external" {
		n.Children = append(n.Children, &TraceNode{Kind: "external", Name: c.Key + "(工作区外的服务)"})
		if !containsStr(t.res.External, c.Key) {
			t.res.External = append(t.res.External, c.Key)
		}
	}
	for _, sid := range l.Servers {
		s := t.ix.Server[sid]
		if s == nil {
			continue
		}
		impl := s.Methods[c.Wire]
		if impl == "" {
			n.Children = append(n.Children, &TraceNode{Kind: "unresolved", Component: s.Component,
				Name: fmt.Sprintf("%s 中没有定位到 %s 的实现函数", s.Component, c.Wire)})
			t.noteComp(s.Component)
			continue
		}
		n.Children = append(n.Children, t.build(impl, "rpc", depth+1, path))
	}
	return n
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Text renders the trace as an indented tree followed by a summary.
func (r *TraceResult) Text() string {
	var b strings.Builder
	for i, root := range r.Roots {
		if i > 0 {
			b.WriteString("\n")
		}
		writeNode(&b, root, "", "", true, "")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "涉及组件(%d):%s\n", len(r.Components), strings.Join(r.Components, " → "))
	if len(r.RPCs) == 0 {
		b.WriteString("没有发现 RPC 调用：这个入口只在本仓库内处理。\n")
	} else {
		fmt.Fprintf(&b, "RPC 调用(%d):\n", len(r.RPCs))
		for _, x := range r.RPCs {
			to := strings.Join(x.To, "、")
			switch x.Status {
			case "external":
				to = "工作区外"
			case "ambiguous":
				to = "多个候选:" + to
			case "missing-method":
				to += "(实现方缺少该方法)"
			}
			fmt.Fprintf(&b, "  %s → %s → %s\n", x.From, x.RPC, to)
		}
	}
	if len(r.External) > 0 {
		fmt.Fprintf(&b, "工作区外的服务:%s(把它们的仓库加入工作区可继续展开)\n", strings.Join(r.External, "、"))
	}
	return b.String()
}

func writeNode(b *strings.Builder, n *TraceNode, prefix, childPrefix string, root bool, parentComp string) {
	b.WriteString(prefix)
	switch n.Kind {
	case "func":
		if root || n.Component != parentComp {
			fmt.Fprintf(b, "[%s] ", n.Component)
		}
		b.WriteString(n.Name)
		switch n.Via {
		case "iface":
			b.WriteString(" (接口调用)")
		case "ref":
			b.WriteString(" (函数引用/回调)")
		}
		if n.Pos != "" {
			fmt.Fprintf(b, "  %s", n.Pos)
		}
		if n.Seen {
			b.WriteString("  ↺ 已在上文展开")
		}
		if n.Truncated {
			b.WriteString("  … 已达深度上限")
		}
	case "rpc":
		fmt.Fprintf(b, "⇒ %s  %s", n.Name, n.Pos)
		if n.Status != "" && n.Status != "ok" {
			fmt.Fprintf(b, "  [%s]", statusText(n.Status))
		}
	default:
		fmt.Fprintf(b, "⊘ %s", n.Name)
	}
	b.WriteString("\n")
	comp := n.Component
	if n.Kind != "func" {
		comp = parentComp
	}
	for i, c := range n.Children {
		last := i == len(n.Children)-1
		branch, next := "├─ ", "│  "
		if last {
			branch, next = "└─ ", "   "
		}
		writeNode(b, c, childPrefix+branch, childPrefix+next, false, comp)
	}
}

// Mermaid renders the trace as a flowchart with one subgraph per component.
func (r *TraceResult) Mermaid() string {
	type node struct{ id, label, comp string }
	nodes := map[string]node{}
	var order []string
	var edges []string
	seenEdge := map[string]bool{}
	id := func(s string) string { return fmt.Sprintf("f%d", len(order)) }
	ensure := func(key, label, comp string) string {
		if n, ok := nodes[key]; ok {
			return n.id
		}
		n := node{id: id(key), label: label, comp: comp}
		nodes[key] = n
		order = append(order, key)
		return n.id
	}
	addEdge := func(e string) {
		if !seenEdge[e] {
			seenEdge[e] = true
			edges = append(edges, e)
		}
	}
	var walk func(n *TraceNode, parent string, pendingRPC string)
	walk = func(n *TraceNode, parent string, pendingRPC string) {
		switch n.Kind {
		case "rpc":
			for _, c := range n.Children {
				walk(c, parent, n.RPC)
			}
			return
		case "external", "unresolved":
			me := ensure("x:"+n.Name, n.Name, "")
			if parent != "" {
				addEdge(fmt.Sprintf("  %s -.->|\"%s\"| %s", parent, esc(pendingRPC), me))
			}
			return
		}
		me := ensure(n.Func, n.Name, n.Component)
		if parent != "" {
			switch {
			case pendingRPC != "":
				addEdge(fmt.Sprintf("  %s ==>|\"%s\"| %s", parent, esc(pendingRPC), me))
			case n.Via == "iface" || n.Via == "ref":
				addEdge(fmt.Sprintf("  %s -.-> %s", parent, me))
			default:
				addEdge(fmt.Sprintf("  %s --> %s", parent, me))
			}
		}
		for _, c := range n.Children {
			walk(c, me, "")
		}
	}
	for _, root := range r.Roots {
		walk(root, "", "")
	}
	var b strings.Builder
	b.WriteString("flowchart TD\n")
	byComp := map[string][]string{}
	var comps []string
	for _, k := range order {
		n := nodes[k]
		if _, ok := byComp[n.comp]; !ok {
			comps = append(comps, n.comp)
		}
		byComp[n.comp] = append(byComp[n.comp], k)
	}
	for ci, c := range comps {
		indent := "  "
		if c != "" {
			fmt.Fprintf(&b, "  subgraph c%d[\"%s\"]\n", ci, esc(c))
			indent = "    "
		}
		for _, k := range byComp[c] {
			n := nodes[k]
			fmt.Fprintf(&b, "%s%s[\"%s\"]\n", indent, n.id, esc(n.label))
		}
		if c != "" {
			b.WriteString("  end\n")
		}
	}
	for _, e := range edges {
		b.WriteString(e + "\n")
	}
	return b.String()
}

func esc(s string) string { return strings.ReplaceAll(s, `"`, "#quot;") }
