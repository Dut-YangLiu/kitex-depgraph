// Package report renders a linked graph as a Markdown report (with a Mermaid
// dependency diagram that GitHub renders) and as a console summary.
package report

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/Dut-YangLiu/kitex-depgraph/internal/link"
	"github.com/Dut-YangLiu/kitex-depgraph/internal/model"
)

// Dep is one component-level dependency: caller component -> IDL service,
// implemented by Targets.
type Dep struct {
	From    string
	Key     string
	Targets []string // implementing components; empty for external services
	Status  string   // ok | ambiguous | external | missing-method
	MatchBy string
	Methods map[string]int // wire method -> number of call sites
	Sites   int
	Sample  string // repo:pos of one call site
}

// Deps aggregates calls into component-level dependencies.
func Deps(g *model.Graph) []*Dep {
	ix := model.NewIndex(g)
	byKey := map[string]*Dep{}
	for _, c := range g.Calls {
		l := ix.Link[c.ID]
		if l == nil {
			continue
		}
		var targets []string
		for _, sid := range l.Servers {
			if s := ix.Server[sid]; s != nil {
				targets = append(targets, s.Component)
			}
		}
		targets = uniqSorted(targets)
		for _, from := range c.UsedBy {
			k := from + "|" + c.Key + "|" + strings.Join(targets, ",") + "|" + l.Status
			d := byKey[k]
			if d == nil {
				d = &Dep{From: from, Key: c.Key, Targets: targets, Status: l.Status, MatchBy: l.MatchBy,
					Methods: map[string]int{}, Sample: c.Repo + ":" + c.Pos}
				byKey[k] = d
			}
			d.Methods[c.Wire]++
			d.Sites++
		}
	}
	out := make([]*Dep, 0, len(byKey))
	for _, d := range byKey {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Markdown renders the full report.
func Markdown(g *model.Graph, version string) string {
	var b strings.Builder
	deps := Deps(g)
	services := 0
	for _, c := range g.Components {
		if c.Kind == "service" {
			services++
		}
	}
	sev := map[string]int{}
	for _, is := range g.Issues {
		sev[is.Severity]++
	}
	sites := 0
	for _, d := range deps {
		sites += d.Sites
	}

	fmt.Fprintf(&b, "# Kitex 服务依赖报告:%s\n\n", g.Workspace)
	fmt.Fprintf(&b, "> 由 kitex-depgraph %s 静态分析生成(不编译、不运行)· 扫描时间 %s · 耗时 %.1fs\n\n", version, g.ScannedAt, g.Stats.Seconds)

	b.WriteString("## 概览\n\n| 指标 | 数量 |\n|---|---|\n")
	fmt.Fprintf(&b, "| 仓库 | %d(%d 个包，%d 个 Go 文件) |\n", g.Stats.Repos, g.Stats.Packages, g.Stats.Files)
	fmt.Fprintf(&b, "| 服务组件 | %d |\n", services)
	fmt.Fprintf(&b, "| 服务实现(NewServer / RegisterService) | %d |\n", len(g.Servers))
	fmt.Fprintf(&b, "| 组件间 RPC 依赖 | %d |\n", len(deps))
	fmt.Fprintf(&b, "| RPC 调用点(已归属到服务组件) | %d |\n", sites)
	fmt.Fprintf(&b, "| 问题 | 高 %d · 中 %d · 提示 %d |\n\n", sev["high"], sev["medium"], sev["info"])

	b.WriteString("## 服务依赖图\n\n")
	if len(deps) == 0 {
		b.WriteString("没有发现组件之间的 RPC 调用。\n\n")
	} else {
		b.WriteString("实线：调用能唯一确定实现方;虚线：工作区外的服务、有多个候选实现方，或实现方缺少被调用的方法。\n\n")
		b.WriteString("```mermaid\n")
		b.WriteString(Mermaid(g, deps))
		b.WriteString("```\n\n")
	}

	b.WriteString("## 跨服务调用明细\n\n")
	if len(deps) > 0 {
		b.WriteString("| 调用方 | 被调服务(IDL) | 实现方 | 方法(调用点数) | 匹配方式 | 示例位置 |\n|---|---|---|---|---|---|\n")
		for _, d := range deps {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | `%s` |\n",
				d.From, d.Key, targetsText(d), methodsText(d.Methods), matchText(d), d.Sample)
		}
		b.WriteString("\n")
	}

	b.WriteString("## 服务实现\n\n")
	if len(g.Servers) > 0 {
		b.WriteString("| 服务(IDL) | 实现组件 | 实现类型 | 注册名 | 已定位方法 | 注册位置 |\n|---|---|---|---|---|---|\n")
		servers := append([]model.Server(nil), g.Servers...)
		sort.Slice(servers, func(i, j int) bool { return servers[i].Key < servers[j].Key })
		svc := map[string]*model.Service{}
		for i := range g.Services {
			svc[g.Services[i].ID] = &g.Services[i]
		}
		for _, s := range servers {
			total := 0
			if sv := svc[s.Service]; sv != nil {
				total = len(sv.Methods)
			}
			fmt.Fprintf(&b, "| %s | %s | `%s` | %s | %d/%d | `%s:%s` |\n",
				s.Key, s.Component, s.Handler, namesText(s.ServerNames), len(s.Methods), total, s.Repo, s.Pos)
		}
		b.WriteString("\n")
	}

	b.WriteString("## 生成代码副本\n\n同一个 IDL 服务可能在多个仓库各生成一份，也可能只在公共 IDL 仓库生成一份。方法集合不一致说明 IDL 不同步。\n\n")
	if len(g.Services) > 0 {
		b.WriteString("| 服务(IDL) | 位置 | Kitex 版本 | 编码 | 方法数 |\n|---|---|---|---|---|\n")
		for i := range g.Services {
			s := &g.Services[i]
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %d |\n", s.Key, link.Where(s), dash(s.KitexVersion), dash(s.Codec), len(s.Methods))
		}
		b.WriteString("\n")
	}

	b.WriteString("## 问题清单\n\n")
	if len(g.Issues) == 0 {
		b.WriteString("没有发现问题。\n\n")
	}
	for _, level := range []struct{ sev, title string }{{"high", "高"}, {"medium", "中"}, {"info", "提示"}} {
		var items []model.Issue
		for _, is := range g.Issues {
			if is.Severity == level.sev {
				items = append(items, is)
			}
		}
		if len(items) == 0 {
			continue
		}
		fmt.Fprintf(&b, "### %s(%d)\n\n", level.title, len(items))
		for _, is := range items {
			fmt.Fprintf(&b, "- **%s**", is.Title)
			if is.Pos != "" {
				fmt.Fprintf(&b, " `%s:%s`", is.Repo, is.Pos)
			}
			if is.Detail != "" {
				fmt.Fprintf(&b, "<br/>%s", is.Detail)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("## 读报告须知\n\n")
	b.WriteString("- **匹配方式**:`共享生成代码` 表示调用方和实现方引用同一个生成包(公共 IDL 仓库);`IDL 身份` 表示两边各自生成代码，按 `PackageName.ServiceName` 和方法名对齐。\n")
	b.WriteString("- **组件**:一个仓库通常就是一个组件;一个仓库里有多个 main 包时按 main 包拆分。共享库(SDK、公共模块)里的调用，会归到实际调用它的服务组件上。\n")
	b.WriteString("- **静态分析的边界**:反射、运行时拼接的方法名或 destService、通过消息队列的异步调用都看不到;接口调用按类层次分析(CHA)展开，可能偏多。\n")
	return b.String()
}

// Mermaid renders the component dependency diagram (flowchart body).
func Mermaid(g *model.Graph, deps []*Dep) string {
	var b strings.Builder
	b.WriteString("flowchart LR\n")
	nodes := map[string]bool{}
	external := map[string]bool{}
	type pair struct{ from, to string }
	labels := map[pair][]string{}
	solid := map[pair]bool{}
	var order []pair
	add := func(p pair, label string, ok bool) {
		if _, seen := labels[p]; !seen {
			order = append(order, p)
		}
		labels[p] = append(labels[p], label)
		if ok {
			solid[p] = true
		}
	}
	for _, d := range deps {
		nodes[d.From] = true
		short := d.Key
		if i := strings.LastIndex(short, "."); i >= 0 {
			short = short[i+1:]
		}
		switch {
		case len(d.Targets) == 0:
			ext := "外部:" + d.Key
			external[ext] = true
			add(pair{d.From, ext}, short, false)
		default:
			label := short
			if d.Status == "missing-method" {
				label += "(缺方法)"
			}
			for _, t := range d.Targets {
				nodes[t] = true
				add(pair{d.From, t}, label, d.Status == "ok")
			}
		}
	}
	for _, n := range model.SortedKeys(nodes) {
		fmt.Fprintf(&b, "  %s[\"%s\"]\n", nodeID(n), esc(n))
	}
	for _, n := range model.SortedKeys(external) {
		fmt.Fprintf(&b, "  %s[/\"%s\"/]\n", nodeID(n), esc(strings.TrimPrefix(n, "外部:")+"(工作区外)"))
	}
	for _, p := range order {
		ls := uniqSorted(labels[p])
		label := strings.Join(ls, "<br/>")
		if len(ls) > 4 {
			label = fmt.Sprintf("%s<br/>等 %d 个服务", strings.Join(ls[:3], "<br/>"), len(ls))
		}
		arrow := "-.->"
		if solid[p] {
			arrow = "-->"
		}
		fmt.Fprintf(&b, "  %s %s|\"%s\"| %s\n", nodeID(p.from), arrow, esc(label), nodeID(p.to))
	}
	return b.String()
}

var nonID = regexp.MustCompile(`[^A-Za-z0-9_]`)

func nodeID(name string) string {
	return "n_" + nonID.ReplaceAllStringFunc(name, func(s string) string {
		return fmt.Sprintf("_%x", []rune(s)[0])
	})
}

func esc(s string) string { return strings.ReplaceAll(s, `"`, "#quot;") }

func targetsText(d *Dep) string {
	switch {
	case len(d.Targets) == 0:
		return "工作区外"
	case d.Status == "ambiguous":
		if len(d.Targets) > 4 {
			return fmt.Sprintf("%d 个候选:%s 等", len(d.Targets), strings.Join(d.Targets[:4], "、"))
		}
		return "多个候选:" + strings.Join(d.Targets, "、")
	case d.Status == "missing-method":
		return strings.Join(d.Targets, "、") + "(缺方法)"
	}
	return strings.Join(d.Targets, "、")
}

func matchText(d *Dep) string {
	switch d.MatchBy {
	case "import-path":
		return "共享生成代码"
	case "idl":
		return "IDL 身份"
	}
	return "-"
}

func methodsText(m map[string]int) string {
	keys := model.SortedKeys(m)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if m[k] > 1 {
			parts = append(parts, fmt.Sprintf("%s(%d)", k, m[k]))
		} else {
			parts = append(parts, k)
		}
	}
	return strings.Join(parts, "、")
}

func namesText(ds []model.Dest) string {
	if len(ds) == 0 {
		return "-"
	}
	var parts []string
	for _, d := range ds {
		parts = append(parts, d.String())
	}
	return strings.Join(parts, "、")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func uniqSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// Summary prints a short console summary after a scan.
func Summary(w io.Writer, g *model.Graph) {
	deps := Deps(g)
	sev := map[string]int{}
	for _, is := range g.Issues {
		sev[is.Severity]++
	}
	fmt.Fprintf(w, "扫描完成:%d 个仓库，%d 个包，%d 个 Go 文件，耗时 %.1fs\n", g.Stats.Repos, g.Stats.Packages, g.Stats.Files, g.Stats.Seconds)
	fmt.Fprintf(w, "发现 %d 个服务实现、%d 处 RPC 调用、%d 条组件间依赖;问题：高 %d · 中 %d · 提示 %d\n",
		len(g.Servers), len(g.Calls), len(deps), sev["high"], sev["medium"], sev["info"])
	if len(deps) > 0 {
		fmt.Fprintln(w, "\n组件间依赖:")
		for i, d := range deps {
			if i >= 30 {
				fmt.Fprintf(w, "  …… 共 %d 条，完整列表见 report.md\n", len(deps))
				break
			}
			fmt.Fprintf(w, "  %s -> %s [%s] %s\n", d.From, d.Key, targetsText(d), methodsText(d.Methods))
		}
	}
	shown := 0
	for _, is := range g.Issues {
		if is.Severity == "info" || shown >= 10 {
			continue
		}
		if shown == 0 {
			fmt.Fprintln(w, "\n需要关注:")
		}
		shown++
		mark := "中"
		if is.Severity == "high" {
			mark = "高"
		}
		fmt.Fprintf(w, "  [%s] %s\n", mark, is.Title)
	}
}
