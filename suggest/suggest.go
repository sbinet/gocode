package suggest

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"io/ioutil"
	"path/filepath"
	"strings"

	"github.com/mdempsky/gocode/lookdot"
)

type Config struct {
	Importer types.Importer
	Logf     func(fmt string, args ...interface{})
}

// Suggest returns a list of suggestion candidates and the length of
// the text that should be replaced, if any.
func (c *Config) Suggest(filename string, data []byte, cursor int) ([]Candidate, int) {
	if cursor < 0 {
		return nil, 0
	}

	fset, pos, pkg := c.analyzePackage(filename, data, cursor)
	scope := pkg.Scope().Innermost(pos)

	ctx, expr, partial := deduceCursorContext(data, cursor)
	b := candidateCollector{
		localpkg: pkg,
		partial:  partial,
		filter:   objectFilters[partial],
	}

	switch ctx {
	case selectContext:
		tv, _ := types.Eval(fset, pkg, pos, expr)
		if lookdot.Walk(&tv, b.appendObject) {
			break
		}

		_, obj := scope.LookupParent(expr, pos)
		if pkgName, isPkg := obj.(*types.PkgName); isPkg {
			c.packageCandidates(pkgName.Imported(), &b)
			break
		}

		return nil, 0

	case compositeLiteralContext:
		tv, _ := types.Eval(fset, pkg, pos, expr)
		if tv.IsType() {
			if _, isStruct := tv.Type.Underlying().(*types.Struct); isStruct {
				c.fieldNameCandidates(tv.Type, &b)
				break
			}
		}

		fallthrough
	default:
		c.scopeCandidates(scope, pos, &b)
	}

	res := b.getCandidates()
	if len(res) == 0 {
		return nil, 0
	}
	return res, len(partial)
}

func (c *Config) analyzePackage(filename string, data []byte, cursor int) (*token.FileSet, token.Pos, *types.Package) {
	// If we're in trailing white space at the end of a scope,
	// sometimes go/types doesn't recognize that variables should
	// still be in scope there.
	filesemi := bytes.Join([][]byte{data[:cursor], []byte(";"), data[cursor:]}, nil)

	fset := token.NewFileSet()
	fileAST, err := parser.ParseFile(fset, filename, filesemi, parser.AllErrors)
	if err != nil {
		c.logParseError("Error parsing input file (outer block)", err)
	}
	pos := fset.File(fileAST.Pos()).Pos(cursor)

	var otherASTs []*ast.File
	for _, otherName := range c.findOtherPackageFiles(filename, fileAST.Name.Name) {
		ast, err := parser.ParseFile(fset, otherName, nil, 0)
		if err != nil {
			c.logParseError("Error parsing other file", err)
		}
		otherASTs = append(otherASTs, ast)
	}

	cfg := types.Config{
		Importer: c.Importer,
		Error:    func(err error) {},
	}
	info := types.Info{
		Scopes: make(map[ast.Node]*types.Scope),
	}
	pkg, _ := cfg.Check("", fset, append(otherASTs, fileAST), &info)

	return fset, pos, pkg
}

func (c *Config) fieldNameCandidates(typ types.Type, b *candidateCollector) {
	s := typ.Underlying().(*types.Struct)
	for i, n := 0, s.NumFields(); i < n; i++ {
		b.appendObject(s.Field(i))
	}
}

func (c *Config) packageCandidates(pkg *types.Package, b *candidateCollector) {
	c.scopeCandidates(pkg.Scope(), token.NoPos, b)
}

func (c *Config) scopeCandidates(scope *types.Scope, pos token.Pos, b *candidateCollector) {
	seen := make(map[string]bool)
	for scope != nil {
		for _, name := range scope.Names() {
			if seen[name] {
				continue
			}
			seen[name] = true
			_, obj := scope.LookupParent(name, pos)
			if obj != nil {
				b.appendObject(obj)
			}
		}
		scope = scope.Parent()
	}
}

func (c *Config) logParseError(intro string, err error) {
	if c.Logf == nil {
		return
	}
	if el, ok := err.(scanner.ErrorList); ok {
		c.Logf("%s:", intro)
		for _, er := range el {
			c.Logf(" %s", er)
		}
	} else {
		c.Logf("%s: %s", intro, err)
	}
}

func (c *Config) findOtherPackageFiles(filename, pkgName string) []string {
	if filename == "" {
		return nil
	}

	dir, file := filepath.Split(filename)
	dents, err := ioutil.ReadDir(dir)
	if err != nil {
		panic(err)
	}
	isTestFile := strings.HasSuffix(file, "_test.go")

	// TODO(mdempsky): Use go/build.(*Context).MatchFile or
	// something to properly handle build tags?
	var out []string
	for _, dent := range dents {
		name := dent.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		if name == file || !strings.HasSuffix(name, ".go") {
			continue
		}
		if !isTestFile && strings.HasSuffix(name, "_test.go") {
			continue
		}

		abspath := filepath.Join(dir, name)
		if pkgNameFor(abspath) == pkgName {
			out = append(out, abspath)
		}
	}

	return out
}

func pkgNameFor(filename string) string {
	file, _ := parser.ParseFile(token.NewFileSet(), filename, nil, parser.PackageClauseOnly)
	return file.Name.Name
}
