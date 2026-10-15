// Analyse with type aliases resolved to their targets, whatever toolchain
// builds this binary.
//
//go:debug gotypesalias=0

// Command kitex-depgraph builds a cross-repository dependency graph of Kitex
// microservices by static analysis, and answers questions on top of it:
// which repositories a feature touches, who calls an RPC, what a change
// affects upstream.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Dut-YangLiu/kitex-depgraph/internal/link"
	"github.com/Dut-YangLiu/kitex-depgraph/internal/model"
	"github.com/Dut-YangLiu/kitex-depgraph/internal/query"
	"github.com/Dut-YangLiu/kitex-depgraph/internal/report"
	"github.com/Dut-YangLiu/kitex-depgraph/internal/scan"
)

var version = "0.1.0"

const usage = `kitex-depgraph:Kitex 微服务跨仓库依赖分析(静态分析，不需要编译和运行)

用法:
  kitex-depgraph scan    [-c workspace.json] [-o 输出目录] [仓库目录...]
  kitex-depgraph report  [-g graph.json] [-o report.md]
  kitex-depgraph callers [-g graph.json] [-json] 服务[/方法]
  kitex-depgraph trace   [-g graph.json] [-format text|mermaid|json] [-full] 函数名片段 | rpc:服务/方法
  kitex-depgraph impact  [-g graph.json] [-repo 仓库] [-json] (-diff 文件|- | -files a.go,b.go | -func 函数名片段)
  kitex-depgraph version

典型流程:
  kitex-depgraph scan -c workspace.json                          # 生成 depgraph-out/graph.json 和 report.md
  kitex-depgraph trace CreateOrder                               # 一个功能点涉及哪些仓库、怎么调用
  kitex-depgraph callers user.UserService/GetUser                # 谁在调用这个接口
  git diff -U0 main | kitex-depgraph impact -repo user-svc -diff -   # 评审时看跨仓影响面

每个子命令加 -h 查看参数。
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "scan":
		err = runScan(args)
	case "report":
		err = runReport(args)
	case "callers":
		err = runCallers(args)
	case "trace":
		err = runTrace(args)
	case "impact":
		err = runImpact(args)
	case "version", "-version", "--version":
		fmt.Println("kitex-depgraph", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		err = fmt.Errorf("未知命令 %q\n\n%s", cmd, usage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func newFlags(name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: kitex-depgraph %s %s\n\n", name, synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// parseInterspersed lets flags appear after positional arguments, so both
// "trace -full CreateOrder" and "trace CreateOrder -full" work.
func parseInterspersed(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		_ = fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			return pos
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func runScan(args []string) error {
	fs := newFlags("scan", "[-c workspace.json] [-o 输出目录] [仓库目录...]")
	cfgPath := fs.String("c", "", "工作区配置文件(JSON);不传时把命令行里的每个目录当作一个仓库")
	out := fs.String("o", "depgraph-out", "输出目录，写入 graph.json 和 report.md")
	verbose := fs.Bool("v", false, "输出详细日志(各包的类型检查错误数等)")
	quiet := fs.Bool("q", false, "只输出结果文件路径")
	dirs := parseInterspersed(fs, args)

	var cfg *scan.Config
	var err error
	switch {
	case *cfgPath != "":
		cfg, err = scan.LoadConfig(*cfgPath)
	case len(dirs) > 0:
		cfg, err = scan.ConfigFromDirs(dirs)
	default:
		fs.Usage()
		return fmt.Errorf("请用 -c 指定工作区配置，或直接列出仓库目录")
	}
	if err != nil {
		return err
	}
	logw := io.Writer(os.Stderr)
	if *quiet {
		logw = io.Discard
	}
	g, warnings, err := scan.Scan(cfg, scan.Options{Verbose: *verbose, Log: logw})
	if err != nil {
		return err
	}
	link.Link(g)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	graphPath := filepath.Join(*out, "graph.json")
	if err := g.Save(graphPath); err != nil {
		return err
	}
	reportPath := filepath.Join(*out, "report.md")
	if err := os.WriteFile(reportPath, []byte(report.Markdown(g, version)), 0o644); err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "注意:", w) // shown even with -q
	}
	if !*quiet {
		fmt.Fprintln(os.Stdout)
		report.Summary(os.Stdout, g)
		fmt.Println()
	}
	fmt.Println("图数据:", graphPath)
	fmt.Println("报告:  ", reportPath)
	return nil
}

func loadGraph(path string) (*model.Index, error) {
	g, err := model.Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("找不到 %s,请先执行 kitex-depgraph scan,或用 -g 指定 graph.json", path)
		}
		return nil, err
	}
	return model.NewIndex(g), nil
}

func runReport(args []string) error {
	fs := newFlags("report", "[-g graph.json] [-o report.md]")
	graph := fs.String("g", "depgraph-out/graph.json", "scan 生成的 graph.json")
	out := fs.String("o", "", "输出文件;不传时打印到标准输出")
	_ = fs.Parse(args)
	ix, err := loadGraph(*graph)
	if err != nil {
		return err
	}
	md := report.Markdown(ix.G, version)
	if *out == "" {
		fmt.Print(md)
		return nil
	}
	return os.WriteFile(*out, []byte(md), 0o644)
}

func runCallers(args []string) error {
	fs := newFlags("callers", "[-g graph.json] [-json] 服务[/方法]   例如 user.UserService/GetUser")
	graph := fs.String("g", "depgraph-out/graph.json", "scan 生成的 graph.json")
	asJSON := fs.Bool("json", false, "输出 JSON")
	rpc := fs.String("rpc", "", "服务或方法(也可以直接写在参数里)")
	pos := parseInterspersed(fs, args)
	if *rpc == "" && len(pos) > 0 {
		*rpc = pos[0]
	}
	if *rpc == "" {
		fs.Usage()
		return fmt.Errorf("请指定服务，例如 user.UserService 或 user.UserService/GetUser")
	}
	ix, err := loadGraph(*graph)
	if err != nil {
		return err
	}
	res, err := query.Callers(ix, *rpc)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(res)
	}
	fmt.Print(res.Text())
	return nil
}

func runTrace(args []string) error {
	fs := newFlags("trace", "[-g graph.json] [-format text|mermaid|json] [-full] [-depth N] 函数名片段 | 仓库:函数名片段 | rpc:服务/方法")
	graph := fs.String("g", "depgraph-out/graph.json", "scan 生成的 graph.json")
	from := fs.String("from", "", "起点(也可以直接写在参数里)")
	format := fs.String("format", "text", "输出格式:text、mermaid 或 json")
	full := fs.Bool("full", false, "保留不通向 RPC 调用的分支(默认只显示通向 RPC 的路径)")
	depth := fs.Int("depth", 40, "函数调用的最大展开深度")
	pos := parseInterspersed(fs, args)
	if *from == "" && len(pos) > 0 {
		*from = strings.Join(pos, " ")
	}
	if *from == "" {
		fs.Usage()
		return fmt.Errorf("请指定起点函数，例如 CreateOrder、gateway:CreateOrder 或 rpc:order.OrderService/CreateOrder")
	}
	ix, err := loadGraph(*graph)
	if err != nil {
		return err
	}
	starts, err := query.FindFuncs(ix, *from)
	if err != nil {
		return err
	}
	if len(starts) > 8 {
		var b strings.Builder
		fmt.Fprintf(&b, "%q 匹配到 %d 个函数，请写得更具体(可加 \"仓库名:\" 前缀):\n", *from, len(starts))
		for i, f := range starts {
			if i >= 20 {
				fmt.Fprintf(&b, "  ……\n")
				break
			}
			fmt.Fprintf(&b, "  [%s] %s  %s:%d\n", f.Component, f.Name, f.File, f.Line)
		}
		return fmt.Errorf("%s", b.String())
	}
	res := query.Trace(ix, starts, query.TraceOptions{MaxDepth: *depth, Full: *full})
	switch *format {
	case "json":
		return printJSON(res)
	case "mermaid":
		fmt.Print(res.Mermaid())
	default:
		fmt.Print(res.Text())
	}
	return nil
}

func runImpact(args []string) error {
	fs := newFlags("impact", "[-g graph.json] [-repo 仓库] [-json] (-diff 文件|- | -files a.go,b.go | -func 函数名片段)")
	graph := fs.String("g", "depgraph-out/graph.json", "scan 生成的 graph.json")
	repo := fs.String("repo", "", "改动所在的仓库名(工作区只有一个仓库时可省略)")
	diff := fs.String("diff", "", "统一 diff 文件，- 表示从标准输入读取(例如 git diff -U0 main | ... -diff -)")
	files := fs.String("files", "", "改动的文件，逗号分隔(相对仓库根目录)")
	fn := fs.String("func", "", "改动的函数(名称片段)")
	hops := fs.Int("hops", 10, "向上游追踪的最大 RPC 层数")
	asJSON := fs.Bool("json", false, "输出 JSON")
	_ = fs.Parse(args)
	if *diff == "" && *files == "" && *fn == "" {
		fs.Usage()
		return fmt.Errorf("请用 -diff、-files 或 -func 说明改了什么")
	}
	ix, err := loadGraph(*graph)
	if err != nil {
		return err
	}
	in := query.ImpactInput{Repo: *repo, MaxHops: *hops}
	if *diff != "" {
		var r io.Reader = os.Stdin
		if *diff != "-" {
			f, err := os.Open(*diff)
			if err != nil {
				return err
			}
			defer f.Close()
			r = f
		}
		if in.Change, err = query.ParseDiff(r); err != nil {
			return err
		}
	}
	if *files != "" {
		for _, f := range strings.Split(*files, ",") {
			if f = strings.TrimSpace(f); f != "" {
				in.Files = append(in.Files, filepath.ToSlash(f))
			}
		}
	}
	if *fn != "" {
		pattern := *fn
		if *repo != "" && !strings.Contains(pattern, ":") {
			pattern = *repo + ":" + pattern
		}
		if in.Funcs, err = query.FindFuncs(ix, pattern); err != nil {
			return err
		}
	}
	res, err := query.Impact(ix, in)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(res)
	}
	fmt.Print(res.Text())
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
