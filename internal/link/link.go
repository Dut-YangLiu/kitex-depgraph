// Package link joins RPC call sites with the servers that implement them,
// across repositories, and reports contract problems.
//
// Join keys:
//
//   - primary: the IDL identity PackageName.ServiceName plus the wire method
//     name, read from each side's own generated code. It works whether the
//     generated code lives in each repository or in a shared IDL repository.
//   - secondary: the destService passed to NewClient versus the names servers
//     register with WithServerBasicInfo (or declare in the config). It picks
//     the right implementation when several repositories implement the same
//     IDL service.
package link

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Dut-YangLiu/kitex-depgraph/internal/model"
)

// Link fills g.Links and the calls' UsedBy, and appends the linking issues
// to g.Issues. It only reads the graph, so it can run again on a loaded
// graph.json.
func Link(g *model.Graph) {
	g.Links = nil
	issues := keepScanIssues(g.Issues)
	attributeCalls(g)

	svcByID := map[string]*model.Service{}
	for i := range g.Services {
		svcByID[g.Services[i].ID] = &g.Services[i]
	}
	serversByKey := map[string][]*model.Server{}
	for i := range g.Servers {
		s := &g.Servers[i]
		serversByKey[s.Key] = append(serversByKey[s.Key], s)
	}

	external := map[compKey]map[string]bool{}
	ambiguous := map[compKey]map[string]bool{}
	dynamic := map[compKey]map[string]bool{}
	missing := map[string]*model.Issue{}
	mismatch := map[string]*model.Issue{}
	called := map[string]bool{}

	for i := range g.Calls {
		c := &g.Calls[i]
		l := model.Link{Call: c.ID}
		cands := serversByKey[c.Key]
		used := len(c.UsedBy) > 0 // calls in unused library code do not raise issues
		if used && c.Dest.Kind == "dynamic" {
			for _, comp := range c.UsedBy {
				addSet(dynamic, compKey{comp, c.Key}, c.Dest.Expr)
			}
		}
		if len(cands) == 0 {
			l.Status = "external"
			for _, comp := range c.UsedBy {
				addSet(external, compKey{comp, c.Key}, c.Wire)
			}
			g.Links = append(g.Links, l)
			continue
		}

		var has []*model.Server
		for _, s := range cands {
			if svc := svcByID[s.Service]; svc == nil || hasWire(svc, c.Wire) {
				has = append(has, s)
			}
		}
		pick := has
		if len(has) == 0 {
			l.Status = "missing-method"
			pick = cands
			l.Note = fmt.Sprintf("实现方的生成代码里没有方法 %s", c.Wire)
			k := c.Component + "|" + c.RPC()
			if used && missing[k] == nil {
				var where []string
				for _, s := range cands {
					where = append(where, s.Component)
				}
				missing[k] = &model.Issue{
					Kind: "missing-method", Severity: "high", Repo: c.Repo, Pos: c.Pos, Key: c.Key,
					Title: fmt.Sprintf("%s 调用了 %s,但实现方(%s)的生成代码里没有这个方法",
						strings.Join(c.UsedBy, "、"), c.RPC(), joinLimit(uniq(where), 4)),
					Detail: "调用方和实现方使用的 IDL 版本不一致：要么实现方还没上线这个方法，要么调用方用了已删除的方法。请对齐两边的 IDL 后重新生成代码。",
				}
			}
		}

		if vals := destValues(c.Dest); len(vals) > 0 {
			var named []*model.Server
			for _, s := range pick {
				if intersects(s.Names(), vals) {
					named = append(named, s)
				}
			}
			switch {
			case len(named) > 0:
				pick = named
			case allNamed(pick):
				k := c.Component + "|" + c.Key + "|" + c.Dest.Value
				if used && mismatch[k] == nil {
					var regs []string
					for _, s := range pick {
						regs = append(regs, s.Component+"="+strings.Join(s.Names(), "/"))
					}
					mismatch[k] = &model.Issue{
						Kind: "name-mismatch", Severity: "medium", Repo: c.Repo, Pos: c.Pos, Key: c.Key,
						Title: fmt.Sprintf("%s 调用 %s 时 destService 为 %q,与实现方注册的服务名不一致",
							strings.Join(c.UsedBy, "、"), c.Key, c.Dest.Value),
						Detail: "实现方注册名:" + joinLimit(regs, 6) + "。如果名字由服务发现或配置映射，可在工作区配置里为实现方补充 serviceNames。",
					}
				}
				if l.Note == "" {
					l.Note = fmt.Sprintf("destService %q 与实现方注册名不一致", c.Dest.Value)
				}
			}
		}

		if l.Status == "" {
			if len(pick) == 1 {
				l.Status = "ok"
			} else {
				l.Status = "ambiguous"
				for _, comp := range c.UsedBy {
					for _, s := range pick {
						addSet(ambiguous, compKey{comp, c.Key}, s.Component)
					}
				}
			}
		}
		l.MatchBy = "idl"
		callSvc := svcByID[c.Service]
		for _, s := range pick {
			l.Servers = append(l.Servers, s.ID)
			if used {
				called[s.ID] = true
			}
			if svc := svcByID[s.Service]; svc != nil && callSvc != nil && svc.ImportPath == callSvc.ImportPath {
				l.MatchBy = "import-path"
			}
		}
		g.Links = append(g.Links, l)
	}

	for _, k := range sortedCompKeys(external) {
		issues = append(issues, model.Issue{
			Kind: "external", Severity: "info", Repo: repoOfComp(g, k.comp), Key: k.key,
			Title:  fmt.Sprintf("%s 调用了 %s,工作区里没有它的实现", k.comp, k.key),
			Detail: "涉及方法:" + joinLimit(model.SortedKeys(external[k]), 8) + "。如果实现方在其他仓库，把那个仓库加进工作区即可串起来。",
		})
	}
	for _, k := range sortedCompKeys(ambiguous) {
		issues = append(issues, model.Issue{
			Kind: "ambiguous", Severity: "medium", Repo: repoOfComp(g, k.comp), Key: k.key,
			Title:  fmt.Sprintf("%s 调用的 %s 有 %d 个候选实现方:%s", k.comp, k.key, len(ambiguous[k]), joinLimit(model.SortedKeys(ambiguous[k]), 4)),
			Detail: "destService 无法静态确定，或者候选实现方没有可区分的注册名。可在工作区配置里为各实现方填写 serviceNames。",
		})
	}
	for _, k := range sortedCompKeys(dynamic) {
		issues = append(issues, model.Issue{
			Kind: "dynamic-dest", Severity: "info", Repo: repoOfComp(g, k.comp), Key: k.key,
			Title:  fmt.Sprintf("%s 调用 %s 的 destService 来自运行时", k.comp, k.key),
			Detail: "表达式:" + joinLimit(model.SortedKeys(dynamic[k]), 4) + "。只按 IDL 身份匹配实现方。",
		})
	}
	for _, k := range model.SortedKeys(missing) {
		issues = append(issues, *missing[k])
	}
	for _, k := range model.SortedKeys(mismatch) {
		issues = append(issues, *mismatch[k])
	}
	issues = append(issues, driftIssues(g)...)
	for _, s := range g.Servers {
		if !called[s.ID] {
			issues = append(issues, model.Issue{
				Kind: "no-caller", Severity: "info", Repo: s.Repo, Pos: s.Pos, Key: s.Key,
				Title:  fmt.Sprintf("%s 提供的 %s 在工作区内没有调用方", s.Component, s.Key),
				Detail: "调用方可能在工作区之外(网关、其他团队的服务)，或者服务已经下线。",
			})
		}
	}
	sortIssues(issues)
	g.Issues = issues
}

// attributeCalls fills Call.UsedBy. A call in a service component belongs to
// that component. A call in library code (a client SDK, a shared module)
// belongs to the service components whose functions reach it through the
// call graph.
func attributeCalls(g *model.Graph) {
	kind := map[string]string{}
	for _, c := range g.Components {
		kind[c.Name] = c.Kind
	}
	funcComp := map[string]string{}
	for _, f := range g.Funcs {
		funcComp[f.ID] = f.Component
	}
	in := map[string][]string{}
	for _, e := range g.Edges {
		if e.To != "" {
			in[e.To] = append(in[e.To], e.From)
		}
	}
	memo := map[string][]string{}
	users := func(start string) []string {
		if u, ok := memo[start]; ok {
			return u
		}
		found := map[string]bool{}
		seen := map[string]bool{start: true}
		queue := []string{start}
		for len(queue) > 0 && len(seen) < 20000 {
			f := queue[0]
			queue = queue[1:]
			for _, caller := range in[f] {
				if seen[caller] {
					continue
				}
				seen[caller] = true
				if comp := funcComp[caller]; kind[comp] == "service" {
					found[comp] = true
					continue
				}
				queue = append(queue, caller)
			}
		}
		u := model.SortedKeys(found)
		memo[start] = u
		return u
	}
	for i := range g.Calls {
		c := &g.Calls[i]
		if kind[c.Component] == "library" {
			c.UsedBy = users(c.Func)
		} else {
			c.UsedBy = []string{c.Component}
		}
	}
}

// keepScanIssues drops issues produced by a previous Link call.
func keepScanIssues(in []model.Issue) []model.Issue {
	var out []model.Issue
	for _, is := range in {
		switch is.Kind {
		case "external", "ambiguous", "dynamic-dest", "missing-method", "name-mismatch", "drift", "no-caller":
			continue
		}
		out = append(out, is)
	}
	return out
}

// driftIssues compares the generated copies of each IDL service.
func driftIssues(g *model.Graph) []model.Issue {
	byKey := map[string][]*model.Service{}
	for i := range g.Services {
		s := &g.Services[i]
		byKey[s.Key] = append(byKey[s.Key], s)
	}
	var out []model.Issue
	for _, key := range model.SortedKeys(byKey) {
		copies := byKey[key]
		if len(copies) < 2 {
			continue
		}
		union := map[string]bool{}
		for _, c := range copies {
			for _, m := range c.Methods {
				union[m.Wire] = true
			}
		}
		differs := false
		var lines []string
		for _, c := range copies {
			var lack []string
			for _, w := range model.SortedKeys(union) {
				if !hasWire(c, w) {
					lack = append(lack, w)
				}
			}
			if len(lack) > 0 {
				differs = true
			}
			line := fmt.Sprintf("%s(Kitex %s,%d 个方法)", Where(c), orDash(c.KitexVersion), len(c.Methods))
			if len(lack) > 0 {
				line += " 缺少 " + joinLimit(lack, 8)
			}
			lines = append(lines, line)
		}
		if !differs {
			continue
		}
		out = append(out, model.Issue{
			Kind: "drift", Severity: "medium", Key: key,
			Title:  fmt.Sprintf("%s 有 %d 份生成代码，方法集合不一致(IDL 不同步)", key, len(copies)),
			Detail: strings.Join(lines, ";"),
		})
	}
	return out
}

// Where describes where a generated copy lives.
func Where(s *model.Service) string {
	if s.Repo != "" {
		return s.Repo + "/" + s.Location
	}
	if s.Source == "modcache" {
		return "模块缓存 " + s.ImportPath
	}
	return s.Source + " " + s.ImportPath
}

func hasWire(s *model.Service, wire string) bool {
	for _, m := range s.Methods {
		if m.Wire == wire {
			return true
		}
	}
	return false
}

func destValues(d model.Dest) []string {
	if d.Value == "" {
		return nil
	}
	switch d.Kind {
	case "const", "var", "inferred", "config", "multiple":
		return strings.Split(d.Value, ",")
	}
	return nil
}

func intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

func allNamed(ss []*model.Server) bool {
	for _, s := range ss {
		if len(s.Names()) == 0 {
			return false
		}
	}
	return len(ss) > 0
}

type compKey struct{ comp, key string }

func addSet(m map[compKey]map[string]bool, k compKey, v string) {
	if m[k] == nil {
		m[k] = map[string]bool{}
	}
	if v != "" {
		m[k][v] = true
	}
}

func sortedCompKeys(m map[compKey]map[string]bool) []compKey {
	keys := make([]compKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].comp != keys[j].comp {
			return keys[i].comp < keys[j].comp
		}
		return keys[i].key < keys[j].key
	})
	return keys
}

func repoOfComp(g *model.Graph, comp string) string {
	for _, c := range g.Components {
		if c.Name == comp {
			return c.Repo
		}
	}
	return comp
}

// joinLimit joins at most n items and says how many there are in total.
func joinLimit(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, "、")
	}
	return fmt.Sprintf("%s 等 %d 个", strings.Join(items[:n], "、"), len(items))
}

func uniq(in []string) []string {
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

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

var severityRank = map[string]int{"high": 0, "medium": 1, "info": 2}

func sortIssues(is []model.Issue) {
	sort.SliceStable(is, func(i, j int) bool {
		a, b := is[i], is[j]
		if severityRank[a.Severity] != severityRank[b.Severity] {
			return severityRank[a.Severity] < severityRank[b.Severity]
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Title < b.Title
	})
}
