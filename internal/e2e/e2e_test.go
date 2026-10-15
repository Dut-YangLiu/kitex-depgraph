// Package e2e runs the whole pipeline on testdata/demo, a small workspace
// that reproduces the situations the tool is meant to catch:
//
//	gateway      BFF; its own (older) generated copy of the order IDL, a
//	             package-level order client, a local user client, the shared SDK
//	order-svc    its own generated order IDL; handler registered through an
//	             interface variable; user client stored in a struct field;
//	             pay client behind an interface; one call with a runtime
//	             destService
//	user-svc     implements user.UserService from the shared IDL repo
//	user-svc-v2  a second implementation registered as "user-v2"
//	common-sdk   shared library wrapping the user client
//	idl          shared IDL repository (user, pay)
package e2e

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Dut-YangLiu/kitex-depgraph/internal/link"
	"github.com/Dut-YangLiu/kitex-depgraph/internal/model"
	"github.com/Dut-YangLiu/kitex-depgraph/internal/query"
	"github.com/Dut-YangLiu/kitex-depgraph/internal/report"
	"github.com/Dut-YangLiu/kitex-depgraph/internal/scan"
)

func scanDemo(t *testing.T) *model.Graph {
	t.Helper()
	cfg, err := scan.LoadConfig("../../testdata/demo/workspace.json")
	if err != nil {
		t.Fatal(err)
	}
	g, warnings, err := scan.Scan(cfg, scan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) > 0 {
		t.Fatalf("warnings: %v", warnings)
	}
	link.Link(g)
	return g
}

func TestScanFindsServicesAndServers(t *testing.T) {
	g := scanDemo(t)

	var copies []string
	for _, s := range g.Services {
		copies = append(copies, s.Key+"@"+link.Where(&s))
	}
	sort.Strings(copies)
	want := []string{
		"order.OrderService@gateway/kitex_gen/order/orderservice",
		"order.OrderService@order-svc/kitex_gen/order/orderservice",
		"pay.PayService@idl/kitex_gen/pay/payservice",
		"user.UserService@idl/kitex_gen/user/userservice",
	}
	if strings.Join(copies, "\n") != strings.Join(want, "\n") {
		t.Errorf("service copies:\n%s\nwant:\n%s", strings.Join(copies, "\n"), strings.Join(want, "\n"))
	}

	servers := map[string]model.Server{}
	for _, s := range g.Servers {
		servers[s.Component+" "+s.Key] = s
	}
	order, ok := servers["order-svc order.OrderService"]
	if !ok {
		t.Fatalf("order-svc server not found: %v", servers)
	}
	if order.HandlerVia != "cha" || order.Handler != "handler.OrderServiceImpl" || len(order.Methods) != 3 {
		t.Errorf("order server = %+v", order)
	}
	if got := order.Names(); len(got) != 1 || got[0] != "order" {
		t.Errorf("order server names = %v (constant from another package)", got)
	}
	if s := servers["user-svc user.UserService"]; s.HandlerVia != "static" || len(s.Methods) != 2 || strings.Join(s.Names(), ",") != "user" {
		t.Errorf("user-svc server = %+v", s)
	}
	if s := servers["user-svc-v2 user.UserService"]; strings.Join(s.Names(), ",") != "user-v2" {
		t.Errorf("user-svc-v2 server = %+v", s)
	}
	pay := g.Services[2]
	if pay.Methods[0].Go != "Prepay" || pay.Methods[0].Wire != "prepay" {
		t.Errorf("pay methods = %+v (wire name differs in case from the Go name)", pay.Methods)
	}
}

func TestLinks(t *testing.T) {
	g := scanDemo(t)
	ix := model.NewIndex(g)
	type row struct{ usedBy, rpc, status, to, dest string }
	var got []string
	for _, c := range g.Calls {
		l := ix.Link[c.ID]
		var to []string
		for _, sid := range l.Servers {
			to = append(to, ix.Server[sid].Component)
		}
		got = append(got, strings.Join([]string{strings.Join(c.UsedBy, ","), c.RPC(), l.Status, strings.Join(to, ","), c.Dest.String()}, " | "))
	}
	sort.Strings(got)
	want := []string{
		" | user.UserService/BatchGetUser | ok | user-svc | user", // unused SDK function
		"gateway | order.OrderService/CreateOrder | ok | order-svc | order",
		"gateway | order.OrderService/GetOrder | ok | order-svc | order",
		"gateway | order.OrderService/QueryOrderLegacy | missing-method | order-svc | order",
		"gateway | user.UserService/GetUser | ok | user-svc | user", // through common-sdk
		"gateway | user.UserService/GetUser | ok | user-svc | user", // local client, var-initialised name
		"order-svc | pay.PayService/prepay | external |  | 动态: cfg.PayService",
		"order-svc | user.UserService/BatchGetUser | ok | user-svc | user",
		"order-svc | user.UserService/GetUser | ambiguous | user-svc,user-svc-v2 | 动态: svcName",
		"order-svc | user.UserService/GetUser | ok | user-svc | user",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("links:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Each side generated its own order IDL: matched by IDL identity. The
	// user service comes from the shared IDL repo: same generated package.
	for _, c := range g.Calls {
		l := ix.Link[c.ID]
		switch c.Key {
		case "order.OrderService":
			if l.MatchBy != "idl" {
				t.Errorf("%s matched by %q, want idl", c.RPC(), l.MatchBy)
			}
		case "user.UserService":
			if l.MatchBy != "import-path" {
				t.Errorf("%s matched by %q, want import-path", c.RPC(), l.MatchBy)
			}
		}
	}
}

func TestIssues(t *testing.T) {
	g := scanDemo(t)
	kinds := map[string]int{}
	for _, is := range g.Issues {
		kinds[is.Severity+"/"+is.Kind]++
	}
	want := map[string]int{
		"high/missing-method": 1,
		"medium/ambiguous":    1,
		"medium/drift":        1,
		"info/external":       1,
		"info/dynamic-dest":   2,
	}
	for k, n := range want {
		if kinds[k] != n {
			t.Errorf("%s: got %d, want %d (all: %v)", k, kinds[k], n, kinds)
		}
	}
	if len(kinds) != len(want) {
		t.Errorf("unexpected issue kinds: %v", kinds)
	}
	if g.Issues[0].Severity != "high" {
		t.Errorf("issues are not sorted by severity: %+v", g.Issues[0])
	}

	// Linking again (as `report` does on a loaded graph) must not duplicate issues.
	n := len(g.Issues)
	link.Link(g)
	if len(g.Issues) != n {
		t.Errorf("relinking changed the issue count: %d -> %d", n, len(g.Issues))
	}
}

func TestComponents(t *testing.T) {
	g := scanDemo(t)
	got := map[string]string{}
	for _, c := range g.Components {
		got[c.Name] = c.Kind
	}
	want := map[string]string{
		"gateway": "service", "order-svc": "service", "user-svc": "service", "user-svc-v2": "service",
		"common-sdk": "library", "idl": "library",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("component %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestTraceCrossesRepositories(t *testing.T) {
	ix := model.NewIndex(scanDemo(t))
	starts, err := query.FindFuncs(ix, "gateway:CreateOrder")
	if err != nil {
		t.Fatal(err)
	}
	res := query.Trace(ix, starts, query.TraceOptions{})
	if len(res.Roots) != 1 || res.Roots[0].Name != "handler.CreateOrder" {
		t.Fatalf("roots: %+v", res.Roots)
	}
	if got := strings.Join(res.Components, ","); got != "gateway,order-svc,user-svc" {
		t.Errorf("components = %s", got)
	}
	if got := strings.Join(res.External, ","); got != "pay.PayService" {
		t.Errorf("external = %s", got)
	}
	text := res.Text()
	for _, s := range []string{
		"⇒ RPC order.OrderService/CreateOrder",
		"[order-svc] (*handler.OrderServiceImpl).CreateOrder",
		"[user-svc] (*main.UserServiceImpl).GetUser",
		"(rpc.PayAdapter).Prepay (接口调用)",
	} {
		if !strings.Contains(text, s) {
			t.Errorf("trace text lacks %q:\n%s", s, text)
		}
	}
	if m := res.Mermaid(); !strings.Contains(m, `==>|"order.OrderService/CreateOrder"|`) || !strings.Contains(m, `subgraph`) {
		t.Errorf("mermaid:\n%s", m)
	}
}

func TestTraceFromRPC(t *testing.T) {
	ix := model.NewIndex(scanDemo(t))
	starts, err := query.FindFuncs(ix, "rpc:order.OrderService/CreateOrder")
	if err != nil || len(starts) != 1 || starts[0].Component != "order-svc" {
		t.Fatalf("starts = %v, %v", starts, err)
	}
}

func TestCallers(t *testing.T) {
	ix := model.NewIndex(scanDemo(t))
	res, err := query.Callers(ix, "user.UserService/GetUser")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Impls) != 2 {
		t.Errorf("implementations = %+v", res.Impls)
	}
	comps := map[string]int{}
	for _, c := range res.Callers {
		comps[c.Component]++
	}
	if comps["gateway"] != 2 || comps["order-svc"] != 2 {
		t.Errorf("callers by component = %v", comps)
	}
	if _, err := query.Callers(ix, "nope.Service"); err == nil {
		t.Error("unknown service should fail")
	}
}

func TestImpactFromDiff(t *testing.T) {
	ix := model.NewIndex(scanDemo(t))
	diff := `--- a/service/user.go
+++ b/service/user.go
@@ -12,3 +12,4 @@
 		return "", err
 	}
+	// trim the name
 	return u.Name, nil
`
	ch, err := query.ParseDiff(strings.NewReader(diff))
	if err != nil {
		t.Fatal(err)
	}
	res, err := query.Impact(ix, query.ImpactInput{Repo: "user-svc", Change: ch})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changed) != 1 || res.Changed[0].Name != "service.LoadUser" {
		t.Fatalf("changed = %+v", res.Changed)
	}
	rpcs := map[string]int{}
	for _, a := range res.RPCs {
		rpcs[a.RPC] = a.Level
	}
	want := map[string]int{
		"user.UserService/GetUser":       1,
		"user.UserService/BatchGetUser":  1,
		"order.OrderService/CreateOrder": 2,
		"order.OrderService/CancelOrder": 2,
	}
	for k, lvl := range want {
		if rpcs[k] != lvl {
			t.Errorf("affected %s: level %d, want %d (all %v)", k, rpcs[k], lvl, rpcs)
		}
	}
	var entries []string
	for _, e := range res.Entries {
		entries = append(entries, e.Component+" "+e.Name)
	}
	for _, e := range []string{"gateway handler.CreateOrder", "gateway handler.GetOrderDetail", "gateway handler.Profile"} {
		if !strings.Contains(strings.Join(entries, "\n"), e) {
			t.Errorf("entries lack %q: %v", e, entries)
		}
	}
	if got := strings.Join(res.Components, ","); got != "gateway,order-svc,user-svc" {
		t.Errorf("components = %s", got)
	}
}

func TestImpactOfIDLChange(t *testing.T) {
	ix := model.NewIndex(scanDemo(t))
	res, err := query.Impact(ix, query.ImpactInput{Repo: "idl", Files: []string{"kitex_gen/user/userservice/client.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.IDL) != 1 || res.IDL[0].Key != "user.UserService" {
		t.Fatalf("idl changes = %+v", res.IDL)
	}
	if got := strings.Join(res.IDL[0].Implementers, ","); got != "user-svc,user-svc-v2" {
		t.Errorf("implementers = %s", got)
	}
}

func TestReportAndRoundTrip(t *testing.T) {
	g := scanDemo(t)
	md := report.Markdown(g, "test")
	for _, s := range []string{
		"```mermaid",
		`n_gateway -->|"OrderService<br/>OrderService(缺方法)"| n_order_2dsvc`,
		"| order-svc | pay.PayService | 工作区外 | prepay |",
		"### 高(1)",
	} {
		if !strings.Contains(md, s) {
			t.Errorf("report lacks %q", s)
		}
	}

	path := filepath.Join(t.TempDir(), "graph.json")
	if err := g.Save(path); err != nil {
		t.Fatal(err)
	}
	g2, err := model.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(g2.Calls) != len(g.Calls) || len(g2.Funcs) != len(g.Funcs) || len(g2.Issues) != len(g.Issues) {
		t.Error("graph.json round trip lost data")
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "null") {
		t.Error("graph.json should contain arrays, not null")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// copyDir copies a directory tree (test helper).
func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

const callerMain = `package main

import (
	"context"

	"example.com/idl/kitex_gen/user"
	"example.com/idl/kitex_gen/user/userservice"
)

func main() {
	cli := userservice.MustNewClient("user")
	_, _ = cli.GetUser(context.Background(), &user.GetUserReq{Id: 1})
}
`

// scanCaller scans a single repo that requires the shared IDL module without
// a replace directive, so the IDL code must come from elsewhere.
func scanCaller(t *testing.T, dir string) *model.Graph {
	t.Helper()
	cfg, err := scan.ConfigFromDirs([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	g, _, err := scan.Scan(cfg, scan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	link.Link(g)
	return g
}

func TestSharedIDLFromModuleCache(t *testing.T) {
	tmp := t.TempDir()
	modcache := filepath.Join(tmp, "modcache")
	copyDir(t, "../../testdata/demo/idl", filepath.Join(modcache, "example.com", "idl@v1.0.0"))
	t.Setenv("GOMODCACHE", modcache)

	repo := filepath.Join(tmp, "caller")
	writeFile(t, filepath.Join(repo, "go.mod"), "module example.com/caller\n\ngo 1.21\n\nrequire example.com/idl v1.0.0\n")
	writeFile(t, filepath.Join(repo, "main.go"), callerMain)

	g := scanCaller(t, repo)
	if len(g.Calls) != 1 || g.Calls[0].RPC() != "user.UserService/GetUser" || g.Calls[0].Dest.Value != "user" {
		t.Fatalf("calls = %+v", g.Calls)
	}
	if len(g.Services) != 1 || g.Services[0].Source != "modcache" {
		t.Fatalf("services = %+v", g.Services)
	}
}

func TestSharedIDLFromVendor(t *testing.T) {
	t.Setenv("GOMODCACHE", t.TempDir()) // empty: must not be needed
	repo := filepath.Join(t.TempDir(), "caller")
	copyDir(t, "../../testdata/demo/idl", filepath.Join(repo, "vendor", "example.com", "idl"))
	writeFile(t, filepath.Join(repo, "vendor", "modules.txt"), "# example.com/idl v1.0.0\n## explicit\nexample.com/idl/kitex_gen/user\nexample.com/idl/kitex_gen/user/userservice\n")
	writeFile(t, filepath.Join(repo, "go.mod"), "module example.com/caller\n\ngo 1.21\n\nrequire example.com/idl v1.0.0\n")
	writeFile(t, filepath.Join(repo, "main.go"), callerMain)

	g := scanCaller(t, repo)
	if len(g.Calls) != 1 || g.Calls[0].RPC() != "user.UserService/GetUser" {
		t.Fatalf("calls = %+v", g.Calls)
	}
	if len(g.Services) != 1 || g.Services[0].Source != "vendor" {
		t.Fatalf("services = %+v", g.Services)
	}
}

func TestWarnsAboutMissingGeneratedCode(t *testing.T) {
	t.Setenv("GOMODCACHE", t.TempDir()) // the IDL module is nowhere to be found
	repo := filepath.Join(t.TempDir(), "caller")
	writeFile(t, filepath.Join(repo, "go.mod"), "module example.com/caller\n\ngo 1.21\n\nrequire example.com/idl v1.0.0\n")
	writeFile(t, filepath.Join(repo, "main.go"), callerMain)
	cfg, err := scan.ConfigFromDirs([]string{repo})
	if err != nil {
		t.Fatal(err)
	}
	g, warnings, err := scan.Scan(cfg, scan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Calls) != 0 {
		t.Fatalf("calls = %+v", g.Calls)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "example.com/idl/kitex_gen/user/userservice") {
		t.Fatalf("warnings = %q", warnings)
	}
}
