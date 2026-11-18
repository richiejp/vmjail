//go:build ignore

// Run from the repository root: go run docs/scope-analysis/analyze.go [integration]
// Produces a conservative typed reference graph, not a runtime execution trace.
package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"golang.org/x/tools/go/packages"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Node struct {
	ID, Name, Package, File, Kind string
	Line                          int
	Test                          bool
	Edges                         []string
}
type Output struct {
	Nodes    map[string]*Node
	Roots    map[string][]string
	Packages map[string][]string
	Errors   []string
}

func main() {
	cwd, _ := os.Getwd()
	fs := token.NewFileSet()
	cfg := &packages.Config{Mode: packages.LoadSyntax, Tests: true, Fset: fs}
	if len(os.Args) > 1 {
		cfg.BuildFlags = []string{"-tags=" + os.Args[1]}
	}
	ps, err := packages.Load(cfg, "./cmd/...", "./internal/...")
	if err != nil {
		panic(err)
	}
	out := Output{Nodes: map[string]*Node{}, Roots: map[string][]string{}, Packages: map[string][]string{}}
	objID := func(o types.Object) string {
		if o == nil || !o.Pos().IsValid() {
			return ""
		}
		p := fs.Position(o.Pos())
		rel, e := filepath.Rel(cwd, p.Filename)
		if e != nil || strings.HasPrefix(rel, "../") {
			return ""
		}
		return fmt.Sprintf("%s:%d:%d", rel, p.Line, p.Column)
	}
	type body struct {
		id  string
		ast ast.Node
		p   *packages.Package
	}
	var bodies []body
	objs := map[string]types.Object{}
	methods := map[string][]*types.Func{}
	iface := map[string]*types.Func{}
	add := func(o types.Object, p *packages.Package, kind string, a ast.Node) string {
		id := objID(o)
		if id == "" {
			return ""
		}
		pos := fs.Position(o.Pos())
		rel, _ := filepath.Rel(cwd, pos.Filename)
		name := o.Name()
		if f, ok := o.(*types.Func); ok {
			if s, ok := f.Type().(*types.Signature); ok && s.Recv() != nil {
				name = types.TypeString(s.Recv().Type(), func(*types.Package) string { return "" }) + "." + name
			}
		}
		out.Nodes[id] = &Node{ID: id, Name: name, Package: strings.Split(p.PkgPath, " [")[0], File: rel, Line: pos.Line, Kind: kind, Test: strings.HasSuffix(rel, "_test.go")}
		objs[id] = o
		if a != nil {
			bodies = append(bodies, body{id, a, p})
		}
		return id
	}
	for _, p := range ps {
		for _, e := range p.Errors {
			out.Errors = append(out.Errors, e.Error())
		}
		if strings.HasSuffix(p.PkgPath, ".test") {
			continue
		}
		var imports []string
		for name := range p.Imports {
			imports = append(imports, name)
		}
		sort.Strings(imports)
		if p.ID == p.PkgPath && !strings.HasSuffix(p.Name, "_test") {
			out.Packages[p.PkgPath] = imports
		}
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				switch d := d.(type) {
				case *ast.FuncDecl:
					o := p.TypesInfo.Defs[d.Name]
					id := add(o, p, "function", d.Body)
					if id == "" {
						continue
					}
					if fn, ok := o.(*types.Func); ok && d.Recv != nil && !out.Nodes[id].Test {
						methods[o.Name()] = append(methods[o.Name()], fn)
					}
				case *ast.GenDecl:
					for _, s := range d.Specs {
						if v, ok := s.(*ast.ValueSpec); ok {
							for _, n := range v.Names {
								if _, ok := p.TypesInfo.Defs[n].(*types.Var); ok {
									add(p.TypesInfo.Defs[n], p, "variable", v)
								}
							}
						}
					}
				}
			}
		}
		// Struct function fields and interface methods are bridge nodes.
		for _, o := range p.TypesInfo.Defs {
			if o == nil {
				continue
			}
			switch o := o.(type) {
			case *types.Var:
				if o.IsField() {
					if _, ok := o.Type().Underlying().(*types.Signature); ok {
						add(o, p, "callback field", nil)
					}
				}
			case *types.Func:
				if s, ok := o.Type().(*types.Signature); ok && s.Recv() != nil {
					if _, ok := s.Recv().Type().Underlying().(*types.Interface); ok {
						id := add(o, p, "interface method", nil)
						iface[id] = o
					}
				}
			}
		}
	}
	edgeSets := map[string]map[string]bool{}
	edge := func(a, b string) {
		if a == "" || b == "" || a == b || out.Nodes[a] == nil || out.Nodes[b] == nil {
			return
		}
		if edgeSets[a] == nil {
			edgeSets[a] = map[string]bool{}
		}
		edgeSets[a][b] = true
	}
	refs := func(a ast.Node, p *packages.Package) []string {
		var r []string
		if a == nil {
			return r
		}
		ast.Inspect(a, func(n ast.Node) bool {
			if i, ok := n.(*ast.Ident); ok {
				if id := objID(p.TypesInfo.Uses[i]); out.Nodes[id] != nil {
					r = append(r, id)
				}
			}
			return true
		})
		return r
	}
	for _, b := range bodies {
		if b.ast == nil {
			continue
		}
		for _, r := range refs(b.ast, b.p) {
			edge(b.id, r)
		}
		if out.Nodes[b.id].Test {
			continue
		}
		ast.Inspect(b.ast, func(n ast.Node) bool {
			if kv, ok := n.(*ast.KeyValueExpr); ok {
				if key, ok := kv.Key.(*ast.Ident); ok {
					id := objID(b.p.TypesInfo.Uses[key])
					if nd := out.Nodes[id]; nd != nil && nd.Kind == "callback field" {
						for _, r := range refs(kv.Value, b.p) {
							edge(id, r)
						}
					}
				}
			}
			return true
		})
	}
	for id, fn := range iface {
		s := fn.Type().(*types.Signature)
		it := s.Recv().Type().Underlying().(*types.Interface)
		for _, m := range methods[fn.Name()] {
			recv := m.Type().(*types.Signature).Recv().Type()
			if types.Implements(recv, it) || types.Implements(types.NewPointer(recv), it) {
				edge(id, objID(m))
			}
		}
	}
	for id, n := range out.Nodes {
		for target := range edgeSets[id] {
			n.Edges = append(n.Edges, target)
		}
		sort.Strings(n.Edges)
		if !n.Test && n.Kind == "function" && n.Name == "main" {
			switch n.File {
			case "cmd/daft/main.go":
				out.Roots["client"] = append(out.Roots["client"], id)
			case "cmd/daft-sandboxd/main.go":
				out.Roots["sandboxd"] = append(out.Roots["sandboxd"], id)
			case "cmd/daft-agent/main.go":
				out.Roots["guest"] = append(out.Roots["guest"], id)
			}
		}
	}
	// Include init and package variables in the import closure: initialization
	// can register callbacks even when main does not name them explicitly.
	for component, roots := range out.Roots {
		seen := map[string]bool{}
		var visit func(string)
		visit = func(p string) {
			if seen[p] {
				return
			}
			seen[p] = true
			for _, i := range out.Packages[p] {
				visit(i)
			}
		}
		for _, r := range roots {
			visit(out.Nodes[r].Package)
		}
		for id, n := range out.Nodes {
			if !n.Test && seen[n.Package] && (n.Kind == "variable" || n.Name == "init") {
				out.Roots[component] = append(out.Roots[component], id)
			}
		}
		sort.Strings(out.Roots[component])
	}
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(out); err != nil {
		panic(err)
	}
}
