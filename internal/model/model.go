// Package model defines the graph.json data model shared by the scanner,
// the linker, the report and the query commands.
package model

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// SchemaVersion is bumped whenever graph.json changes incompatibly.
const SchemaVersion = 1

// Graph is everything one scan produces. It is self-contained: every query
// command works from graph.json alone, without the source code.
type Graph struct {
	SchemaVersion int         `json:"schemaVersion"`
	Tool          string      `json:"tool"`
	Workspace     string      `json:"workspace"`
	ScannedAt     string      `json:"scannedAt"`
	Repos         []Repo      `json:"repos"`
	Components    []Component `json:"components"`
	Services      []Service   `json:"services"`
	Servers       []Server    `json:"servers"`
	Clients       []Client    `json:"clients"`
	Calls         []Call      `json:"calls"`
	Links         []Link      `json:"links"`
	Funcs         []Func      `json:"funcs"`
	Edges         []Edge      `json:"edges"`
	Issues        []Issue     `json:"issues"`
	Stats         Stats       `json:"stats"`
}

// Repo is one scanned repository.
type Repo struct {
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	Modules    []string `json:"modules"`
	Components []string `json:"components"`
	Packages   int      `json:"packages"`
	Files      int      `json:"files"`
	TypeErrors int      `json:"typeErrors"`
}

// Component is a deployable unit: a repo, or one main package of a repo
// that holds several. Kind "service" has a main package; "library" is shared
// code (a client SDK, a utilities module) whose RPC calls belong to whoever
// calls into it.
type Component struct {
	Name string `json:"name"`
	Repo string `json:"repo"`
	Dir  string `json:"dir"`
	Kind string `json:"kind"` // service | library
}

// Method maps the Go method name of a Kitex client to the method name sent
// on the wire (the IDL method name). They can differ in case, e.g. Echo/echo.
type Method struct {
	Go   string `json:"go"`
	Wire string `json:"wire"`
}

// Service is one generated copy of a Kitex service package
// (kitex_gen/<ns>/<service>). The same IDL service usually has several
// copies: one per repository that generates code locally, or a single copy in
// a shared IDL repository.
type Service struct {
	ID           string   `json:"id"`
	Key          string   `json:"key"` // PackageName.ServiceName, e.g. user.UserService
	PackageName  string   `json:"packageName"`
	ServiceName  string   `json:"serviceName"`
	ImportPath   string   `json:"importPath"`
	Repo         string   `json:"repo,omitempty"` // empty when outside the workspace
	Location     string   `json:"location"`       // repo-relative dir, or absolute path outside the workspace
	Source       string   `json:"source"`         // workspace | vendor | modcache | local
	KitexVersion string   `json:"kitexVersion,omitempty"`
	Codec        string   `json:"codec,omitempty"` // Thrift | Protobuf
	IDLFile      string   `json:"idlFile,omitempty"`
	Methods      []Method `json:"methods"`
}

// Dest describes the destService argument passed to NewClient, or a server
// name registered with WithServerBasicInfo.
type Dest struct {
	Value string `json:"value,omitempty"`
	// Kind: const (compile-time constant), var (package var with a literal
	// initializer), inferred (only one candidate in the component), config
	// (declared in the workspace config), dynamic (runtime value),
	// multiple (several candidates), unknown.
	Kind string `json:"kind"`
	Expr string `json:"expr,omitempty"`
}

// Known reports whether the destination has a concrete value.
func (d Dest) Known() bool { return d.Value != "" && d.Kind != "dynamic" && d.Kind != "multiple" }

func (d Dest) String() string {
	switch {
	case d.Value != "" && d.Kind == "multiple":
		return d.Value + "(多个候选)"
	case d.Value != "":
		return d.Value
	case d.Expr != "":
		return "动态: " + d.Expr
	default:
		return "未知"
	}
}

// Server is one service registration: a call to <service>.NewServer or
// <service>.RegisterService with a handler.
type Server struct {
	ID          string            `json:"id"`
	Repo        string            `json:"repo"`
	Component   string            `json:"component"`
	Key         string            `json:"key"`
	Service     string            `json:"service"` // Service.ID of the generated package used
	Handler     string            `json:"handler"`
	HandlerVia  string            `json:"handlerVia"` // static | cha | unknown
	ServerNames []Dest            `json:"serverNames,omitempty"`
	Pos         string            `json:"pos"`
	Methods     map[string]string `json:"methods"` // wire method -> Func.ID of the implementation
}

// Names returns the known registered service names.
func (s Server) Names() []string {
	var out []string
	for _, d := range s.ServerNames {
		if d.Known() {
			out = append(out, d.Value)
		}
	}
	return out
}

// Client is one call to <service>.NewClient / MustNewClient.
type Client struct {
	Repo      string `json:"repo"`
	Component string `json:"component"`
	Key       string `json:"key"`
	Service   string `json:"service"`
	Dest      Dest   `json:"dest"`
	Holder    string `json:"holder,omitempty"`
	Pos       string `json:"pos"`
}

// Call is one RPC call site: a method call on a Kitex generated Client.
type Call struct {
	ID        string `json:"id"`
	Repo      string `json:"repo"`
	Component string `json:"component"`
	Func      string `json:"func"` // Func.ID of the enclosing function
	Pos       string `json:"pos"`
	Key       string `json:"key"`
	Service   string `json:"service"`
	Method    string `json:"method"` // Go method name
	Wire      string `json:"wire"`   // IDL method name
	Dest      Dest   `json:"dest"`
	// UsedBy lists the service components the call belongs to: the call's
	// own component, or for a call inside library code, the service
	// components whose functions reach it. Empty means unused library code.
	UsedBy []string `json:"usedBy"`
}

// RPC returns the "Key/Wire" identifier of the called method.
func (c Call) RPC() string { return c.Key + "/" + c.Wire }

// Link resolves one call to the server(s) that implement it.
type Link struct {
	Call    string   `json:"call"`
	Servers []string `json:"servers,omitempty"`
	// Status: ok | ambiguous | external | missing-method
	Status  string `json:"status"`
	MatchBy string `json:"matchBy,omitempty"` // import-path | idl
	Note    string `json:"note,omitempty"`
}

// Func is a node of the intra-repo call graph.
type Func struct {
	ID        string `json:"id"`
	Repo      string `json:"repo"`
	Component string `json:"component"`
	Name      string `json:"name"` // short display name
	File      string `json:"file"`
	Line      int    `json:"line"`
	EndLine   int    `json:"endLine"`
}

// Edge is a call-graph edge. Kind: call (static call), iface (interface call
// resolved by class hierarchy analysis), ref (function used as a value),
// rpc (Kitex client call; To is empty and Call is set).
type Edge struct {
	From string `json:"from"`
	To   string `json:"to,omitempty"`
	Call string `json:"call,omitempty"`
	Kind string `json:"kind"`
	Line int    `json:"line"`
}

// Issue is a finding worth a human look.
type Issue struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"` // high | medium | info
	Title    string `json:"title"`
	Detail   string `json:"detail,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Pos      string `json:"pos,omitempty"`
	Key      string `json:"key,omitempty"`
}

// Stats summarises the scan.
type Stats struct {
	Repos          int     `json:"repos"`
	Packages       int     `json:"packages"`
	Files          int     `json:"files"`
	ExternalLoaded int     `json:"externalLoaded"`
	FakedImports   int     `json:"fakedImports"`
	TypeErrors     int     `json:"typeErrors"`
	Funcs          int     `json:"funcs"`
	Edges          int     `json:"edges"`
	Seconds        float64 `json:"seconds"`
}

// Load reads graph.json.
func Load(path string) (*Graph, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var g Graph
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	if g.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%s 的格式版本是 %d,当前工具需要 %d,请重新执行 scan", path, g.SchemaVersion, SchemaVersion)
	}
	return &g, nil
}

// Save writes graph.json.
func (g *Graph) Save(path string) error {
	g.normalize()
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// normalize replaces nil slices with empty ones so graph.json always has
// arrays, which is friendlier to other tools.
func (g *Graph) normalize() {
	if g.Repos == nil {
		g.Repos = []Repo{}
	}
	if g.Components == nil {
		g.Components = []Component{}
	}
	if g.Services == nil {
		g.Services = []Service{}
	}
	if g.Servers == nil {
		g.Servers = []Server{}
	}
	if g.Clients == nil {
		g.Clients = []Client{}
	}
	if g.Calls == nil {
		g.Calls = []Call{}
	}
	if g.Links == nil {
		g.Links = []Link{}
	}
	if g.Funcs == nil {
		g.Funcs = []Func{}
	}
	if g.Edges == nil {
		g.Edges = []Edge{}
	}
	if g.Issues == nil {
		g.Issues = []Issue{}
	}
	for i := range g.Calls {
		if g.Calls[i].UsedBy == nil {
			g.Calls[i].UsedBy = []string{}
		}
	}
	for i := range g.Servers {
		if g.Servers[i].Methods == nil {
			g.Servers[i].Methods = map[string]string{}
		}
	}
}

// Index gives fast lookups over a Graph. Build it after linking.
type Index struct {
	G        *Graph
	Service  map[string]*Service
	Server   map[string]*Server
	Call     map[string]*Call
	Link     map[string]*Link // by call ID
	Func     map[string]*Func
	Out      map[string][]Edge // by Edge.From
	In       map[string][]Edge // by Edge.To (rpc edges excluded)
	CallsBy  map[string][]*Call
	ServerOf map[string][]*Server // by Func.ID of an implementing method
}

// NewIndex builds the lookup tables.
func NewIndex(g *Graph) *Index {
	ix := &Index{
		G:        g,
		Service:  map[string]*Service{},
		Server:   map[string]*Server{},
		Call:     map[string]*Call{},
		Link:     map[string]*Link{},
		Func:     map[string]*Func{},
		Out:      map[string][]Edge{},
		In:       map[string][]Edge{},
		CallsBy:  map[string][]*Call{},
		ServerOf: map[string][]*Server{},
	}
	for i := range g.Services {
		ix.Service[g.Services[i].ID] = &g.Services[i]
	}
	for i := range g.Servers {
		s := &g.Servers[i]
		ix.Server[s.ID] = s
		for _, fid := range s.Methods {
			ix.ServerOf[fid] = append(ix.ServerOf[fid], s)
		}
	}
	for i := range g.Calls {
		c := &g.Calls[i]
		ix.Call[c.ID] = c
		ix.CallsBy[c.Func] = append(ix.CallsBy[c.Func], c)
	}
	for i := range g.Links {
		ix.Link[g.Links[i].Call] = &g.Links[i]
	}
	for i := range g.Funcs {
		ix.Func[g.Funcs[i].ID] = &g.Funcs[i]
	}
	for _, e := range g.Edges {
		ix.Out[e.From] = append(ix.Out[e.From], e)
		if e.To != "" {
			ix.In[e.To] = append(ix.In[e.To], e)
		}
	}
	return ix
}

// FuncName returns a readable name for a Func ID, falling back to the ID.
func (ix *Index) FuncName(id string) string {
	if f := ix.Func[id]; f != nil {
		return f.Name
	}
	if i := strings.Index(id, ":"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// ComponentOfFunc returns the component of a Func ID.
func (ix *Index) ComponentOfFunc(id string) string {
	if f := ix.Func[id]; f != nil {
		return f.Component
	}
	return ""
}

// SortedKeys returns the keys of a string-keyed map in order.
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
