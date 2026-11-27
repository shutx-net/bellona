// Package archtest holds the architecture tests of Bellona. It consists of
// this test file only: every rule is a function over the parsed Go sources of
// the module, and each rule is unit-tested against synthetic sources before
// it is applied to the real tree. The rules keep the dependency direction of
// the packages, the purity of the packages that replay depends on, the
// separation of the core from command-line I/O, and the promise that
// Bellona never deletes or truncates user data on its own.
package archtest

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Dependency matrix
// ---------------------------------------------------------------------------

// dependencyMatrix lists, for every package of the module that lives under
// internal/, cmd/ or roles/, the module-internal packages that its non-test
// files may import. Keys and values are directories relative to the module
// root. A package that is missing here fails the architecture test: whoever
// adds a package decides its place in the layering and records it here.
//
// internal/engine is the only non-test importer of internal/storage, so only
// the engine can commit events or write artifacts (R-FSM-03, R-PER-03).
var dependencyMatrix = map[string][]string{
	"internal/strictjson": {},
	"internal/domain":     {"internal/strictjson"},
	"internal/artifact":   {"internal/domain", "internal/strictjson"},
	"internal/version":    {},
	"internal/fsm":        {"internal/domain"},
	"internal/projection": {"internal/domain", "internal/fsm", "internal/strictjson"},
	"internal/storage":    {"internal/domain", "internal/strictjson"},
	"internal/config":     {"internal/domain", "internal/strictjson"},
	"internal/manifest": {
		"internal/config", "internal/domain", "internal/fsm", "internal/strictjson", "internal/version",
	},
	"roles":                     {"internal/domain"},
	"internal/agent":            {"internal/domain"},
	"internal/runtime/fake":     {"internal/agent", "internal/domain"},
	"internal/evals":            {"internal/strictjson"},
	"internal/testutil/gitrepo": {},
	"internal/engine": {
		"internal/agent", "internal/artifact", "internal/config", "internal/domain", "internal/fsm",
		"internal/manifest", "internal/projection", "internal/storage", "internal/strictjson", "roles",
		"internal/version",
	},
	"cmd/bellona": {"internal/domain", "internal/fsm", "internal/version"},
}

// managedTrees are the top-level directories whose packages must appear in
// the dependency matrix.
var managedTrees = []string{"internal", "cmd", "roles"}

// testOnlyPackages may be imported by _test.go files only.
var testOnlyPackages = []string{"internal/testutil", "internal/runtime/fake"}

// purePackages are the packages that state reconstruction depends on. They do
// no I/O, read no clock and draw no randomness (R-PER-06).
var purePackages = []string{
	"internal/strictjson", "internal/domain", "internal/artifact", "internal/fsm", "internal/projection",
}

// bannedPureImports are the standard-library packages that the pure packages
// must not import, besides everything under net/.
var bannedPureImports = []string{
	"os", "os/exec", "os/signal", "os/user", "syscall", "io/fs", "io/ioutil", "path/filepath", "net",
	"math/rand", "math/rand/v2", "crypto/rand", "log", "log/slog", "flag", "unsafe",
}

// bannedPureTimeFuncs are the members of package time that read the clock or
// wait.
var bannedPureTimeFuncs = []string{
	"Now", "Since", "Until", "Sleep", "After", "AfterFunc", "Tick", "NewTicker", "NewTimer",
}

// bannedCLIImports and bannedCLIRefs keep command-line I/O out of internal/
// (R-CLI-04): the core receives its input and writes its output through
// values that cmd/ passes in.
var (
	bannedCLIImports = []string{"flag", "log"}
	bannedCLIRefs    = map[string][]string{
		"os":  {"Stdin", "Stdout", "Stderr", "Args", "Exit"},
		"fmt": {"Print", "Printf", "Println", "Scan", "Scanf", "Scanln"},
		"log/slog": {
			"Default", "SetDefault", "Debug", "Info", "Warn", "Error", "Log", "LogAttrs",
			"DebugContext", "InfoContext", "WarnContext", "ErrorContext",
		},
	}
)

// journalFile is the only file that may call a Truncate or Ftruncate method:
// the journal recovers an incomplete trailing record under the Run lock
// (SPEC §12).
const journalFile = "internal/storage/journal.go"

// ---------------------------------------------------------------------------
// Loading the module
// ---------------------------------------------------------------------------

// goFile is one parsed Go source file of the module.
type goFile struct {
	Dir  string // slash-separated directory relative to the module root; "" for the root
	Name string // slash-separated path relative to the module root
	Test bool   // a _test.go file
	File *ast.File
	Fset *token.FileSet
}

func (f goFile) pos(n ast.Node) string { return f.Fset.Position(n.Pos()).String() }

// moduleRoot returns the directory that holds go.mod, searching upward from
// the working directory.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found above the working directory")
		}
		dir = parent
	}
}

// modulePath returns the module path declared by the module directive of a
// go.mod file.
func modulePath(goMod string) (string, error) {
	for line := range strings.Lines(goMod) {
		fields := strings.Fields(stripComment(line))
		if len(fields) >= 2 && fields[0] == "module" {
			return strings.Trim(fields[1], `"`), nil
		}
	}
	return "", errors.New("no module directive in go.mod")
}

func stripComment(line string) string {
	if i := strings.Index(line, "//"); i >= 0 {
		line = line[:i]
	}
	return strings.TrimSpace(line)
}

// loadModule parses every .go file of the module rooted at root. It skips
// .git, directories named testdata, directories whose name starts with "." or
// "_", and directories below the root that hold their own go.mod.
func loadModule(root string) ([]goFile, error) {
	var files []goFile
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			name := d.Name()
			if name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, rel, src, 0)
		if err != nil {
			return err
		}
		dir := path.Dir(rel)
		if dir == "." {
			dir = ""
		}
		files = append(files, goFile{
			Dir: dir, Name: rel, Test: strings.HasSuffix(rel, "_test.go"), File: af, Fset: fset,
		})
		return nil
	})
	return files, err
}

// ---------------------------------------------------------------------------
// Helpers shared by the rules
// ---------------------------------------------------------------------------

func importPath(spec *ast.ImportSpec) string {
	p, err := strconv.Unquote(spec.Path.Value)
	if err != nil {
		return spec.Path.Value
	}
	return p
}

// localName returns the identifier by which a file refers to the imported
// package: the explicit name, else the last element of the path, skipping a
// trailing major-version element such as v2.
func localName(spec *ast.ImportSpec) string {
	if spec.Name != nil {
		return spec.Name.Name
	}
	elems := strings.Split(importPath(spec), "/")
	last := elems[len(elems)-1]
	if len(elems) > 1 && len(last) > 1 && last[0] == 'v' && strings.Trim(last[1:], "0123456789") == "" {
		last = elems[len(elems)-2]
	}
	return last
}

// moduleRel returns the directory, relative to the module root, of an import
// path that lies inside the module.
func moduleRel(importPath, modulePath string) (string, bool) {
	switch {
	case importPath == modulePath:
		return "", true
	case strings.HasPrefix(importPath, modulePath+"/"):
		return strings.TrimPrefix(importPath, modulePath+"/"), true
	}
	return "", false
}

func isStdlib(importPath string) bool {
	first, _, _ := strings.Cut(importPath, "/")
	return !strings.Contains(first, ".")
}

func inTree(dir, tree string) bool { return dir == tree || strings.HasPrefix(dir, tree+"/") }

func inAnyTree(dir string, trees []string) bool {
	return slices.ContainsFunc(trees, func(t string) bool { return inTree(dir, t) })
}

func nonTest(files []goFile) []goFile {
	return slices.DeleteFunc(slices.Clone(files), func(f goFile) bool { return f.Test })
}

// hasDotImport reports whether f imports pkg with a dot, which hides every
// reference to it from a selector search.
func hasDotImport(f goFile, pkg string) bool {
	return slices.ContainsFunc(f.File.Imports, func(s *ast.ImportSpec) bool {
		return importPath(s) == pkg && s.Name != nil && s.Name.Name == "."
	})
}

// forbidRefs reports every selector expression in f that names one of the
// banned members of an imported package, resolving the local name of the
// import (so a renamed import is caught), and every dot import of such a
// package. banned maps an import path to its banned members.
func forbidRefs(f goFile, banned map[string][]string, why string) []string {
	var out []string
	pkgs := make([]string, 0, len(banned))
	for pkg := range banned {
		pkgs = append(pkgs, pkg)
	}
	slices.Sort(pkgs)
	for _, pkg := range pkgs {
		if hasDotImport(f, pkg) {
			out = append(out, fmt.Sprintf("%s: dot import of %s hides references to it; %s", f.Name, pkg, why))
		}
		for _, spec := range f.File.Imports {
			local := localName(spec)
			if importPath(spec) != pkg || local == "_" || local == "." {
				continue
			}
			ast.Inspect(f.File, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == local && slices.Contains(banned[pkg], sel.Sel.Name) {
					out = append(out, fmt.Sprintf("%s: %s.%s is not allowed here; %s", f.pos(sel), pkg, sel.Sel.Name, why))
				}
				return true
			})
		}
	}
	return out
}

// forbidSelectors reports every selector expression in f whose member name is
// banned, whatever it is applied to. banned reports whether a name is banned.
func forbidSelectors(f goFile, banned func(name string) bool, why string) []string {
	var out []string
	ast.Inspect(f.File, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && banned(sel.Sel.Name) {
			out = append(out, fmt.Sprintf("%s: a selector named %s is not allowed here; %s", f.pos(sel), sel.Sel.Name, why))
		}
		return true
	})
	return out
}

// ---------------------------------------------------------------------------
// Rules
// ---------------------------------------------------------------------------

// ruleNoRequire (R-TECH-01): go.mod has no require directive.
func ruleNoRequire(goMod string) []string {
	var out []string
	for i, line := range slices.Collect(strings.Lines(goMod)) {
		line = stripComment(line)
		fields := strings.Fields(line)
		if len(fields) > 0 && (fields[0] == "require" || strings.HasPrefix(fields[0], "require(")) {
			out = append(out, fmt.Sprintf("go.mod:%d: require directive %q: the module has no third-party dependency", i+1, line))
		}
	}
	return out
}

// ruleNoJSONv1: encoding/json (v1) is imported nowhere in the module, test
// files included. Contracts use encoding/json/v2 and encoding/json/jsontext
// through internal/strictjson (R-ART-01).
func ruleNoJSONv1(files []goFile) []string {
	var out []string
	for _, f := range files {
		for _, spec := range f.File.Imports {
			if importPath(spec) == "encoding/json" {
				out = append(out, fmt.Sprintf("%s: encoding/json (v1) is not used; use encoding/json/v2 and encoding/json/jsontext, through internal/strictjson for contracts", f.pos(spec)))
			}
		}
	}
	return out
}

// ruleStdlibOrModule (R-TECH-01): every import of every file, test files
// included, is a standard-library package or lies inside the module.
func ruleStdlibOrModule(files []goFile, modPath string) []string {
	var out []string
	for _, f := range files {
		for _, spec := range f.File.Imports {
			p := importPath(spec)
			if _, inside := moduleRel(p, modPath); inside || isStdlib(p) {
				continue
			}
			out = append(out, fmt.Sprintf("%s: import %q is neither standard library nor inside the module", f.pos(spec), p))
		}
	}
	return out
}

// ruleDependencyMatrix: every package under internal/, cmd/ and roles/ is in
// the matrix, and its non-test files import only the module packages that the
// matrix allows.
func ruleDependencyMatrix(files []goFile, modPath string, matrix map[string][]string) []string {
	var out []string
	reported := make(map[string]bool)
	for _, f := range nonTest(files) {
		top, _, _ := strings.Cut(f.Dir, "/")
		if !slices.Contains(managedTrees, top) {
			continue
		}
		allowed, ok := matrix[f.Dir]
		if !ok {
			if !reported[f.Dir] {
				reported[f.Dir] = true
				out = append(out, fmt.Sprintf("%s: package %s is not in the dependency matrix of internal/archtest; add it with the packages it may import", f.Name, f.Dir))
			}
			continue
		}
		for _, spec := range f.File.Imports {
			rel, inside := moduleRel(importPath(spec), modPath)
			if inside && !slices.Contains(allowed, rel) {
				out = append(out, fmt.Sprintf("%s: %s must not import %s (allowed: %s)", f.pos(spec), f.Dir, rel, strings.Join(allowed, ", ")))
			}
		}
	}
	return out
}

// ruleStorageImporters (R-FSM-03): internal/storage is imported by no
// non-test file except those of internal/engine.
func ruleStorageImporters(files []goFile, modPath string) []string {
	var out []string
	for _, f := range nonTest(files) {
		if inTree(f.Dir, "internal/engine") || inTree(f.Dir, "internal/storage") {
			continue
		}
		for _, spec := range f.File.Imports {
			if rel, inside := moduleRel(importPath(spec), modPath); inside && inTree(rel, "internal/storage") {
				out = append(out, fmt.Sprintf("%s: only internal/engine may import internal/storage, so that only the engine can commit events and write artifacts", f.pos(spec)))
			}
		}
	}
	return out
}

// ruleTestOnlyImports: internal/testutil/... and internal/runtime/fake are
// imported by _test.go files only.
func ruleTestOnlyImports(files []goFile, modPath string) []string {
	var out []string
	for _, f := range nonTest(files) {
		for _, spec := range f.File.Imports {
			if rel, inside := moduleRel(importPath(spec), modPath); inside && inAnyTree(rel, testOnlyPackages) {
				out = append(out, fmt.Sprintf("%s: %s may be imported by _test.go files only", f.pos(spec), rel))
			}
		}
	}
	return out
}

// rulePurity (R-PER-06): the non-test files of the pure packages import none
// of the packages that give access to the outside world and call no function
// that reads the clock or waits.
func rulePurity(files []goFile) []string {
	const why = "this package must stay pure so that replay has no side effects (R-PER-06)"
	var out []string
	for _, f := range nonTest(files) {
		if !inAnyTree(f.Dir, purePackages) {
			continue
		}
		for _, spec := range f.File.Imports {
			p := importPath(spec)
			if slices.Contains(bannedPureImports, p) || strings.HasPrefix(p, "net/") {
				out = append(out, fmt.Sprintf("%s: import %q is not allowed; %s", f.pos(spec), p, why))
			}
		}
		out = append(out, forbidRefs(f, map[string][]string{"time": bannedPureTimeFuncs}, why)...)
	}
	return out
}

// ruleNoCLIIO (R-CLI-04): the non-test files under internal/ neither import
// flag or log nor touch the process's standard streams, arguments or exit,
// nor print, nor use the global log/slog logger.
func ruleNoCLIIO(files []goFile) []string {
	const why = "internal packages do no command-line I/O; cmd/ passes readers, writers and loggers in (R-CLI-04)"
	var out []string
	for _, f := range nonTest(files) {
		if !inTree(f.Dir, "internal") {
			continue
		}
		for _, spec := range f.File.Imports {
			if p := importPath(spec); slices.Contains(bannedCLIImports, p) {
				out = append(out, fmt.Sprintf("%s: import %q is not allowed; %s", f.pos(spec), p, why))
			}
		}
		out = append(out, forbidRefs(f, bannedCLIRefs, why)...)
	}
	return out
}

// ruleNoDeletion (R-PER-10): the non-test files under internal/ and cmd/ never
// remove or truncate anything: no selector named Remove, RemoveAll, Unlink,
// Unlinkat or Rmdir (which also covers the syscall functions of those names),
// and no selector named Truncate or Ftruncate outside journalFile. Bellona
// deletes plans, events and logs only on an explicit user command, which is
// not part of the MVP. The files of
// internal/storage and internal/engine also never create-or-truncate a file:
// they write through temporary files and atomic renames, and the store
// constructors are named CreateRun and OpenRun for that reason.
func ruleNoDeletion(files []goFile) []string {
	const why = "Bellona never deletes or truncates data automatically (R-PER-10)"
	var out []string
	for _, f := range nonTest(files) {
		if !inTree(f.Dir, "internal") && !inTree(f.Dir, "cmd") {
			continue
		}
		guarded := inTree(f.Dir, "internal/storage") || inTree(f.Dir, "internal/engine")
		for _, pkg := range []string{"os", "syscall"} {
			if hasDotImport(f, pkg) {
				out = append(out, fmt.Sprintf("%s: dot import of %s hides references to it; %s", f.Name, pkg, why))
			}
		}
		out = append(out, forbidSelectors(f, func(name string) bool {
			switch name {
			case "Remove", "RemoveAll", "Unlink", "Unlinkat", "Rmdir":
				return true
			case "Truncate", "Ftruncate":
				return f.Name != journalFile
			case "Create", "WriteFile":
				return guarded
			}
			return false
		}, why)...)
		qualified := map[string][]string{}
		if f.Name == journalFile {
			qualified["os"] = []string{"Truncate"} // the journal may truncate a file it holds, not call os.Truncate
		}
		if guarded {
			qualified["os"] = append(qualified["os"], "O_TRUNC")
			qualified["syscall"] = []string{"O_TRUNC"}
		}
		out = append(out, forbidRefs(f, qualified, why)...)
	}
	return out
}

// ---------------------------------------------------------------------------
// The architecture test on the real tree
// ---------------------------------------------------------------------------

func TestArchitecture(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	modPath, err := modulePath(string(goMod))
	if err != nil {
		t.Fatal(err)
	}
	files, err := loadModule(root)
	if err != nil {
		t.Fatalf("parse the module: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found; the loader is broken")
	}
	rules := []struct {
		name string
		run  func() []string
	}{
		{"R-TECH-01 go.mod has no require directive", func() []string { return ruleNoRequire(string(goMod)) }},
		{"R-TECH-01 imports are standard library or module-internal", func() []string { return ruleStdlibOrModule(files, modPath) }},
		{"R-ART-01 no encoding/json v1", func() []string { return ruleNoJSONv1(files) }},
		{"dependency matrix", func() []string { return ruleDependencyMatrix(files, modPath, dependencyMatrix) }},
		{"R-FSM-03 only the engine imports storage", func() []string { return ruleStorageImporters(files, modPath) }},
		{"test-only packages", func() []string { return ruleTestOnlyImports(files, modPath) }},
		{"R-PER-06 purity", func() []string { return rulePurity(files) }},
		{"R-CLI-04 no CLI I/O in internal", func() []string { return ruleNoCLIIO(files) }},
		{"R-PER-10 no deletion or truncation", func() []string { return ruleNoDeletion(files) }},
	}
	for _, r := range rules {
		t.Run(r.name, func(t *testing.T) {
			for _, v := range r.run() {
				t.Error(v)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Unit tests of the rules against synthetic sources
// ---------------------------------------------------------------------------

const testModule = "example.com/bellona"

// synth parses source as the file name relative to the module root.
func synth(t *testing.T, name, src string) goFile {
	t.Helper()
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	dir := path.Dir(name)
	if dir == "." {
		dir = ""
	}
	return goFile{Dir: dir, Name: name, Test: strings.HasSuffix(name, "_test.go"), File: af, Fset: fset}
}

// wantViolations checks that the rule reported exactly n violations and that
// the first one mentions each of the given fragments.
func wantViolations(t *testing.T, got []string, n int, fragments ...string) {
	t.Helper()
	if len(got) != n {
		t.Fatalf("got %d violations, want %d:\n%s", len(got), n, strings.Join(got, "\n"))
	}
	for _, frag := range fragments {
		if !strings.Contains(strings.Join(got, "\n"), frag) {
			t.Errorf("violations do not mention %q:\n%s", frag, strings.Join(got, "\n"))
		}
	}
}

func TestRuleNoRequire(t *testing.T) {
	wantViolations(t, ruleNoRequire("module example.com/m\n\ngo 1.27.3\n"), 0)
	wantViolations(t, ruleNoRequire("module example.com/m\n// require example.com/x v1.0.0\n"), 0)
	wantViolations(t, ruleNoRequire("module example.com/m\n\nrequire example.com/x v1.0.0\n"), 1, "go.mod:3")
	wantViolations(t, ruleNoRequire("module example.com/m\n\nrequire (\n\texample.com/x v1.0.0\n)\n"), 1, "go.mod:3")
	wantViolations(t, ruleNoRequire("module m\nrequire(\n\texample.com/x v1\n)\n"), 1, "go.mod:2")
	wantViolations(t, ruleNoRequire("module m\n\trequire example.com/x v1 // indirect\n"), 1)
}

func TestRuleStdlibOrModule(t *testing.T) {
	clean := synth(t, "internal/a/a.go", `package a
import (
	"fmt"
	"encoding/json/v2"
	"example.com/bellona/internal/b"
	"example.com/bellona"
)`)
	wantViolations(t, ruleStdlibOrModule([]goFile{clean}, testModule), 0)

	thirdParty := synth(t, "internal/a/b.go", `package a
import "github.com/spf13/cobra"`)
	wantViolations(t, ruleStdlibOrModule([]goFile{thirdParty}, testModule), 1, "github.com/spf13/cobra")

	inTest := synth(t, "internal/a/c_test.go", `package a
import _ "golang.org/x/sys/unix"`)
	wantViolations(t, ruleStdlibOrModule([]goFile{inTest}, testModule), 1, "golang.org/x/sys/unix")

	// A path that merely starts like the module path is outside it.
	lookalike := synth(t, "internal/a/d.go", `package a
import "example.com/bellona-evil/x"`)
	wantViolations(t, ruleStdlibOrModule([]goFile{lookalike}, testModule), 1)
}

func TestRuleNoJSONv1(t *testing.T) {
	v1 := synth(t, "internal/a/a.go", `package a
import "encoding/json"`)
	wantViolations(t, ruleNoJSONv1([]goFile{v1}), 1, `encoding/json (v1)`)
	renamed := synth(t, "internal/a/b_test.go", `package a
import j "encoding/json"`)
	wantViolations(t, ruleNoJSONv1([]goFile{renamed}), 1)

	v2 := synth(t, "internal/a/c.go", `package a
import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"encoding/hex"
)`)
	wantViolations(t, ruleNoJSONv1([]goFile{v2}), 0)
}

func TestRuleDependencyMatrix(t *testing.T) {
	matrix := map[string][]string{
		"internal/low":  {},
		"internal/high": {"internal/low"},
	}
	ok := []goFile{
		synth(t, "internal/low/low.go", `package low`),
		synth(t, "internal/high/high.go", `package high
import (
	"fmt"
	"example.com/bellona/internal/low"
)`),
	}
	wantViolations(t, ruleDependencyMatrix(ok, testModule, matrix), 0)

	upward := []goFile{synth(t, "internal/low/low.go", `package low
import "example.com/bellona/internal/high"`)}
	wantViolations(t, ruleDependencyMatrix(upward, testModule, matrix), 1, "internal/low must not import internal/high")

	unknown := []goFile{
		synth(t, "internal/newpkg/a.go", `package newpkg`),
		synth(t, "internal/newpkg/b.go", `package newpkg`),
	}
	wantViolations(t, ruleDependencyMatrix(unknown, testModule, matrix), 1, "package internal/newpkg is not in the dependency matrix")

	for _, name := range []string{"cmd/tool/main.go", "roles/roles.go", "internal/x/y/z.go"} {
		wantViolations(t, ruleDependencyMatrix([]goFile{synth(t, name, `package p`)}, testModule, matrix), 1, "not in the dependency matrix")
	}

	// Test files and packages outside internal/, cmd/ and roles/ are not governed.
	exempt := []goFile{
		synth(t, "internal/low/low_test.go", `package low
import "example.com/bellona/internal/high"`),
		synth(t, "tools/gen/gen.go", `package main
import "example.com/bellona/internal/high"`),
		synth(t, "internal/archtest/only_test.go", `package archtest`),
	}
	wantViolations(t, ruleDependencyMatrix(exempt, testModule, matrix), 0)

	// Importing the module root is not allowed unless listed.
	root := []goFile{synth(t, "internal/low/low.go", `package low
import _ "example.com/bellona"`)}
	wantViolations(t, ruleDependencyMatrix(root, testModule, matrix), 1)
}

func TestRuleStorageImporters(t *testing.T) {
	const storageImport = `package p
import "example.com/bellona/internal/storage"`
	for _, name := range []string{"internal/engine/engine.go", "internal/engine/sub/x.go", "internal/storage/other.go", "internal/engine/e_test.go", "internal/config/c_test.go"} {
		wantViolations(t, ruleStorageImporters([]goFile{synth(t, name, storageImport)}, testModule), 0)
	}
	for _, name := range []string{"internal/projection/p.go", "cmd/bellona/main.go", "tools/x/x.go", "internal/manifest/m.go"} {
		wantViolations(t, ruleStorageImporters([]goFile{synth(t, name, storageImport)}, testModule), 1, "only internal/engine may import internal/storage")
	}
	nested := synth(t, "internal/config/c.go", `package config
import "example.com/bellona/internal/storage/sub"`)
	wantViolations(t, ruleStorageImporters([]goFile{nested}, testModule), 1)
}

func TestRuleTestOnlyImports(t *testing.T) {
	prod := synth(t, "internal/engine/engine.go", `package engine
import (
	"example.com/bellona/internal/testutil/gitrepo"
	"example.com/bellona/internal/runtime/fake"
)`)
	wantViolations(t, ruleTestOnlyImports([]goFile{prod}, testModule), 2, "internal/testutil/gitrepo", "internal/runtime/fake")

	inTest := synth(t, "internal/engine/engine_test.go", `package engine_test
import (
	"example.com/bellona/internal/testutil/gitrepo"
	"example.com/bellona/internal/runtime/fake"
)`)
	wantViolations(t, ruleTestOnlyImports([]goFile{inTest}, testModule), 0)

	other := synth(t, "internal/agent/agent.go", `package agent
import "example.com/bellona/internal/runtime"`)
	wantViolations(t, ruleTestOnlyImports([]goFile{other}, testModule), 0)
}

func TestRulePurity(t *testing.T) {
	bad := []struct{ name, src, want string }{
		{"internal/domain/a.go", "package domain\nimport \"os\"", `"os"`},
		{"internal/domain/a.go", "package domain\nimport \"os/exec\"", `"os/exec"`},
		{"internal/domain/a.go", "package domain\nimport \"os/signal\"", `"os/signal"`},
		{"internal/domain/a.go", "package domain\nimport \"os/user\"", `"os/user"`},
		{"internal/domain/a.go", "package domain\nimport \"syscall\"", `"syscall"`},
		{"internal/domain/a.go", "package domain\nimport \"io/fs\"", `"io/fs"`},
		{"internal/domain/a.go", "package domain\nimport \"io/ioutil\"", `"io/ioutil"`},
		{"internal/domain/a.go", "package domain\nimport \"path/filepath\"", `"path/filepath"`},
		{"internal/domain/a.go", "package domain\nimport \"net\"", `"net"`},
		{"internal/domain/a.go", "package domain\nimport \"net/http\"", `"net/http"`},
		{"internal/domain/a.go", "package domain\nimport \"net/http/httptest\"", `"net/http/httptest"`},
		{"internal/domain/a.go", "package domain\nimport \"math/rand\"", `"math/rand"`},
		{"internal/domain/a.go", "package domain\nimport \"math/rand/v2\"", `"math/rand/v2"`},
		{"internal/artifact/a.go", "package artifact\nimport \"crypto/rand\"", `"crypto/rand"`},
		{"internal/fsm/a.go", "package fsm\nimport \"log\"", `"log"`},
		{"internal/fsm/a.go", "package fsm\nimport \"log/slog\"", `"log/slog"`},
		{"internal/projection/a.go", "package projection\nimport \"flag\"", `"flag"`},
		{"internal/strictjson/a.go", "package strictjson\nimport \"unsafe\"", `"unsafe"`},
		{"internal/domain/sub/a.go", "package sub\nimport \"os\"", `"os"`},
		{"internal/domain/a.go", "package domain\nimport \"time\"\nfunc f() { _ = time.Now() }", "time.Now"},
		{"internal/domain/a.go", "package domain\nimport \"time\"\nfunc f(t time.Time) { _ = time.Since(t) }", "time.Since"},
		{"internal/domain/a.go", "package domain\nimport \"time\"\nfunc f(t time.Time) { _ = time.Until(t) }", "time.Until"},
		{"internal/domain/a.go", "package domain\nimport \"time\"\nfunc f() { time.Sleep(1) }", "time.Sleep"},
		{"internal/domain/a.go", "package domain\nimport \"time\"\nfunc f() { <-time.After(1) }", "time.After"},
		{"internal/domain/a.go", "package domain\nimport \"time\"\nfunc f() { time.AfterFunc(1, nil) }", "time.AfterFunc"},
		{"internal/domain/a.go", "package domain\nimport \"time\"\nfunc f() { _ = time.Tick(1) }", "time.Tick"},
		{"internal/domain/a.go", "package domain\nimport \"time\"\nfunc f() { _ = time.NewTicker(1) }", "time.NewTicker"},
		{"internal/domain/a.go", "package domain\nimport \"time\"\nfunc f() { _ = time.NewTimer(1) }", "time.NewTimer"},
		{"internal/domain/a.go", "package domain\nimport clock \"time\"\nfunc f() { _ = clock.Now() }", "time.Now"},
		{"internal/domain/a.go", "package domain\nimport \"time\"\nvar now = time.Now", "time.Now"},
		{"internal/domain/a.go", "package domain\nimport . \"time\"\nfunc f() { _ = Now() }", "dot import of time"},
	}
	for _, tt := range bad {
		wantViolations(t, rulePurity([]goFile{synth(t, tt.name, tt.src)}), 1, tt.want)
	}

	clean := []struct{ name, src string }{
		{"internal/domain/a.go", "package domain\nimport (\"time\"; \"fmt\"; \"regexp\"; \"io\"; \"crypto/sha256\"; \"path\")\nfunc f(t time.Time, d time.Duration) string { return fmt.Sprint(t.Add(d), time.Second, t.UTC(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) }"},
		{"internal/domain/a.go", "package domain\nimport clock \"time\"\nfunc f() clock.Duration { return clock.Second }"},
		{"internal/domain/a.go", "package domain\nimport \"time\"\ntype T struct{ Now func() time.Time }\nfunc (t T) f() time.Time { return t.Now() }"},
		{"internal/domain/a.go", "package domain\nfunc f() { time := 1; _ = time }"},
		{"internal/engine/a.go", "package engine\nimport (\"os\"; \"time\"; \"math/rand/v2\")\nfunc f() { _ = time.Now(); _ = os.Args; _ = rand.IntN(3) }"},
		{"internal/domain/a_test.go", "package domain\nimport (\"os\"; \"time\"; \"math/rand/v2\")\nfunc f() { _ = time.Now(); _ = os.Args; _ = rand.IntN(3) }"},
		{"internal/domainx/a.go", "package domainx\nimport \"os\""},
	}
	for _, tt := range clean {
		wantViolations(t, rulePurity([]goFile{synth(t, tt.name, tt.src)}), 0)
	}
}

func TestRuleNoCLIIO(t *testing.T) {
	bad := []struct{ name, src, want string }{
		{"internal/engine/a.go", "package engine\nimport \"flag\"", `"flag"`},
		{"internal/engine/a.go", "package engine\nimport \"log\"", `"log"`},
		{"internal/engine/a.go", "package engine\nimport \"os\"\nfunc f() { _ = os.Stdin }", "os.Stdin"},
		{"internal/engine/a.go", "package engine\nimport \"os\"\nfunc f() { _ = os.Stdout }", "os.Stdout"},
		{"internal/engine/a.go", "package engine\nimport \"os\"\nfunc f() { _ = os.Stderr }", "os.Stderr"},
		{"internal/engine/a.go", "package engine\nimport \"os\"\nfunc f() { _ = os.Args }", "os.Args"},
		{"internal/engine/a.go", "package engine\nimport \"os\"\nfunc f() { os.Exit(1) }", "os.Exit"},
		{"internal/engine/a.go", "package engine\nimport \"fmt\"\nfunc f() { fmt.Print(1) }", "fmt.Print"},
		{"internal/engine/a.go", "package engine\nimport \"fmt\"\nfunc f() { fmt.Printf(\"\") }", "fmt.Printf"},
		{"internal/engine/a.go", "package engine\nimport \"fmt\"\nfunc f() { fmt.Println() }", "fmt.Println"},
		{"internal/engine/a.go", "package engine\nimport \"fmt\"\nfunc f() { fmt.Scan() }", "fmt.Scan"},
		{"internal/engine/a.go", "package engine\nimport \"fmt\"\nfunc f() { fmt.Scanf(\"\") }", "fmt.Scanf"},
		{"internal/engine/a.go", "package engine\nimport \"fmt\"\nfunc f() { fmt.Scanln() }", "fmt.Scanln"},
		{"internal/engine/a.go", "package engine\nimport \"fmt\"\nimport \"os\"\nfunc f() { fmt.Fprintln(os.Stderr, 1) }", "os.Stderr"},
		{"internal/engine/a.go", "package engine\nimport \"log/slog\"\nfunc f() { slog.Info(\"\") }", "slog.Info"},
		{"internal/engine/a.go", "package engine\nimport \"log/slog\"\nfunc f() { slog.Error(\"\") }", "slog.Error"},
		{"internal/engine/a.go", "package engine\nimport \"log/slog\"\nfunc f() { slog.SetDefault(nil) }", "slog.SetDefault"},
		{"internal/engine/a.go", "package engine\nimport \"log/slog\"\nfunc f() { _ = slog.Default() }", "slog.Default"},
		{"internal/engine/a.go", "package engine\nimport l \"log/slog\"\nfunc f() { l.Warn(\"\") }", "slog.Warn"},
		{"internal/engine/a.go", "package engine\nimport (\"context\"; \"log/slog\")\nfunc f() { slog.DebugContext(context.Background(), \"\") }", "slog.DebugContext"},
		{"internal/engine/a.go", "package engine\nimport s \"os\"\nfunc f() { _ = s.Args }", "os.Args"},
		{"internal/engine/a.go", "package engine\nimport . \"os\"", "dot import of os"},
		{"internal/testutil/gitrepo/a.go", "package gitrepo\nimport \"os\"\nfunc f() { _ = os.Stdout }", "os.Stdout"},
	}
	for _, tt := range bad {
		wantViolations(t, ruleNoCLIIO([]goFile{synth(t, tt.name, tt.src)}), 1, tt.want)
	}

	clean := []struct{ name, src string }{
		{"internal/engine/a.go", "package engine\nimport (\"fmt\"; \"io\"; \"os\"; \"log/slog\"; \"errors\")\nfunc f(w io.Writer, l *slog.Logger, err error) { fmt.Fprintf(w, \"%s\", err.Error()); _ = fmt.Sprintf(\"\"); l.Info(\"x\"); l.Error(\"y\"); _ = slog.New(nil); _ = slog.LevelInfo; _ = os.Getenv(\"X\"); _ = errors.New(\"\") }"},
		{"internal/engine/a.go", "package engine\ntype T struct{ Stdout, Stderr int }\nfunc f(t T) int { return t.Stdout + t.Stderr }"},
		{"internal/engine/a.go", "package engine\nfunc f() { os := 1; _ = os }"},
		{"internal/engine/a_test.go", "package engine\nimport (\"fmt\"; \"os\"; \"log\")\nfunc f() { fmt.Println(os.Args); log.Println() }"},
		{"cmd/bellona/main.go", "package main\nimport (\"fmt\"; \"os\"; \"flag\"; \"log\")\nfunc main() { fmt.Println(os.Args); flag.Parse(); log.Println(); os.Exit(2) }"},
	}
	for _, tt := range clean {
		wantViolations(t, ruleNoCLIIO([]goFile{synth(t, tt.name, tt.src)}), 0)
	}
}

func TestRuleNoDeletion(t *testing.T) {
	bad := []struct{ name, src, want string }{
		{"internal/domain/a.go", "package domain\nimport \"os\"\nfunc f() { os.Remove(\"x\") }", "named Remove"},
		{"internal/domain/a.go", "package domain\nimport \"os\"\nfunc f() { os.RemoveAll(\"x\") }", "named RemoveAll"},
		{"internal/domain/a.go", "package domain\nimport \"os\"\nfunc f() { os.Truncate(\"x\", 0) }", "named Truncate"},
		{"internal/domain/a.go", "package domain\nimport \"syscall\"\nfunc f() { syscall.Unlink(\"x\") }", "named Unlink"},
		{"internal/domain/a.go", "package domain\nimport \"syscall\"\nfunc f() { syscall.Unlinkat(0, \"x\") }", "named Unlinkat"},
		{"internal/domain/a.go", "package domain\nimport \"syscall\"\nfunc f() { syscall.Rmdir(\"x\") }", "named Rmdir"},
		{"internal/domain/a.go", "package domain\nimport \"syscall\"\nfunc f() { syscall.Ftruncate(0, 0) }", "named Ftruncate"},
		{"internal/domain/a.go", "package domain\nimport s \"syscall\"\nfunc f() { s.Unlink(\"x\") }", "named Unlink"},
		{"cmd/bellona/main.go", "package main\nimport \"syscall\"\nfunc main() { syscall.Rmdir(\"x\") }", "named Rmdir"},
		{"cmd/bellona/main.go", "package main\nimport \"syscall\"\nfunc main() { syscall.Ftruncate(0, 0) }", "named Ftruncate"},
		{"internal/workspace/a.go", "package workspace\nfunc f(x interface{ Unlinkat(int, string) error }) { x.Unlinkat(0, \"p\") }", "named Unlinkat"},
		{"internal/engine/a.go", "package engine\nimport \"syscall\"\nfunc f() { syscall.Ftruncate(0, 0) }", "named Ftruncate"},
		{"internal/engine/a.go", "package engine\nimport \"syscall\"\nfunc f() { syscall.Unlink(\"x\") }", "named Unlink"},
		{"internal/storage/store.go", "package storage\nfunc f(file interface{ Ftruncate(int, int64) error }) { file.Ftruncate(0, 0) }", "named Ftruncate"},
		{"internal/storage/artifacts.go", "package storage\nimport \"syscall\"\nfunc f() { syscall.Unlinkat(0, \"x\") }", "named Unlinkat"},
		{"internal/storage/journal.go", "package storage\nimport \"syscall\"\nfunc f() { syscall.Unlink(\"x\") }", "named Unlink"},
		{"internal/storage/journal.go", "package storage\nimport \"syscall\"\nfunc f() { syscall.Rmdir(\"x\") }", "named Rmdir"},
		{"cmd/bellona/main.go", "package main\nimport \"os\"\nfunc main() { os.RemoveAll(\"x\") }", "named RemoveAll"},
		{"internal/workspace/a.go", "package workspace\ntype fsys interface{ Remove(string) error }\nfunc f(x fsys) { x.Remove(\"p\") }", "named Remove"},
		{"internal/workspace/a.go", "package workspace\nfunc f(x interface{ RemoveAll(string) error }) { x.RemoveAll(\"p\") }", "named RemoveAll"},
		{"internal/engine/a.go", "package engine\nfunc f(file interface{ Truncate(int64) error }) { file.Truncate(0) }", "named Truncate"},
		{"internal/storage/store.go", "package storage\nfunc f(file interface{ Truncate(int64) error }) { file.Truncate(0) }", "named Truncate"},
		{"internal/storage/journal.go", "package storage\nimport \"os\"\nfunc f() { os.Truncate(\"x\", 0) }", "os.Truncate"},
		{"internal/storage/store.go", "package storage\nimport \"os\"\nfunc f() { os.Create(\"x\") }", "named Create"},
		{"internal/storage/store.go", "package storage\nimport \"os\"\nfunc f() { os.WriteFile(\"x\", nil, 0) }", "named WriteFile"},
		{"internal/storage/store.go", "package storage\nimport \"os\"\nfunc f() { os.OpenFile(\"x\", os.O_WRONLY|os.O_TRUNC, 0) }", "os.O_TRUNC"},
		{"internal/storage/store.go", "package storage\nimport \"syscall\"\nfunc f() int { return syscall.O_TRUNC }", "syscall.O_TRUNC"},
		{"internal/storage/store.go", "package storage\nimport \"os\"\ntype w interface{ Create(string) error }\nfunc f(x w) { x.Create(\"p\") }", "named Create"},
		{"internal/engine/a.go", "package engine\nimport \"os\"\nfunc f() { os.WriteFile(\"x\", nil, 0) }", "named WriteFile"},
		{"internal/engine/a.go", "package engine\nimport \"os\"\nfunc f() { os.Create(\"x\") }", "named Create"},
		{"internal/engine/a.go", "package engine\nimport \"os\"\nvar _ = os.O_TRUNC", "os.O_TRUNC"},
		{"internal/engine/a.go", "package engine\nimport \"syscall\"\nvar _ = syscall.O_TRUNC", "syscall.O_TRUNC"},
		{"internal/engine/a.go", "package engine\nimport s \"syscall\"\nvar _ = s.O_TRUNC", "syscall.O_TRUNC"},
		{"internal/domain/a.go", "package domain\nimport . \"os\"", "dot import of os"},
		{"cmd/bellona/main.go", "package main\nimport . \"syscall\"", "dot import of syscall"},
	}
	for _, tt := range bad {
		wantViolations(t, ruleNoDeletion([]goFile{synth(t, tt.name, tt.src)}), 1, tt.want)
	}

	clean := []struct{ name, src string }{
		// The journal may call a Truncate method on the file it holds, and
		// Ftruncate where Truncate is allowed.
		{"internal/storage/journal.go", "package storage\nfunc f(file interface{ Truncate(int64) error }) { file.Truncate(0) }"},
		{"internal/storage/journal.go", "package storage\nimport \"syscall\"\nfunc f() { syscall.Ftruncate(0, 0) }"},
		// Atomic writes use temporary files and renames.
		{"internal/storage/store.go", "package storage\nimport \"os\"\nfunc f() { os.CreateTemp(\"\", \"x\"); os.Rename(\"a\", \"b\"); os.OpenFile(\"x\", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); os.Mkdir(\"d\", 0o700) }"},
		// Only storage and engine are restricted in how they create files.
		{"internal/testutil/gitrepo/a.go", "package gitrepo\nimport \"os\"\nfunc f() { os.WriteFile(\"x\", nil, 0o644); os.Create(\"y\"); _ = os.O_TRUNC }"},
		{"internal/config/a.go", "package config\nimport \"os\"\nfunc f() { os.WriteFile(\"x\", nil, 0o644) }"},
		{"cmd/bellona/main.go", "package main\nimport \"os\"\nfunc main() { os.WriteFile(\"x\", nil, 0o644) }"},
		// Test files and code outside internal/ and cmd/ may delete.
		{"internal/storage/store_test.go", "package storage\nimport \"os\"\nfunc f() { os.RemoveAll(\"x\"); os.Create(\"y\"); os.Truncate(\"z\", 0) }"},
		{"internal/storage/store_test.go", "package storage\nimport \"syscall\"\nfunc f() { syscall.Unlink(\"x\"); syscall.Unlinkat(0, \"x\"); syscall.Rmdir(\"x\"); syscall.Ftruncate(0, 0) }"},
		{"tools/gen/gen.go", "package main\nimport \"os\"\nfunc main() { os.RemoveAll(\"x\") }"},
		{"tools/gen/gen.go", "package main\nimport \"syscall\"\nfunc main() { syscall.Unlink(\"x\"); syscall.Rmdir(\"y\"); syscall.Ftruncate(0, 0) }"},
		{"internal/domain/a.go", "package domain\nimport \"strings\"\nfunc f() string { return strings.TrimSpace(\"x\") }"},
	}
	for _, tt := range clean {
		wantViolations(t, ruleNoDeletion([]goFile{synth(t, tt.name, tt.src)}), 0)
	}
}

// ---------------------------------------------------------------------------
// Unit tests of the loader and of the matrix
// ---------------------------------------------------------------------------

func TestModulePath(t *testing.T) {
	for in, want := range map[string]string{
		"module example.com/m\n\ngo 1.27.3\n":        "example.com/m",
		"// c\nmodule   example.com/m // trailing\n": "example.com/m",
		"module \"example.com/quoted\"\ngo 1.27.3\n": "example.com/quoted",
		"go 1.27.3\nmodule github.com/shutx-net/b\n": "github.com/shutx-net/b",
	} {
		if got, err := modulePath(in); err != nil || got != want {
			t.Errorf("modulePath(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	if _, err := modulePath("go 1.27.3\n"); err == nil {
		t.Error("modulePath accepted a go.mod without a module directive")
	}
}

func TestLocalName(t *testing.T) {
	for in, want := range map[string]string{
		`import "fmt"`:                      "fmt",
		`import "log/slog"`:                 "slog",
		`import "math/rand/v2"`:             "rand",
		`import "encoding/json/v2"`:         "json",
		`import "os/exec"`:                  "exec",
		`import t "time"`:                   "t",
		`import . "time"`:                   ".",
		`import _ "embed"`:                  "_",
		`import "example.com/m/internal/v"`: "v",
	} {
		f := synth(t, "x.go", "package p\n"+in)
		if got := localName(f.File.Imports[0]); got != want {
			t.Errorf("localName(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadModule(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/m\n")
	write("main.go", "package main\n")
	write("internal/a/a.go", "package a\nimport \"os\"\n")
	write("internal/a/a_test.go", "package a\n")
	write("internal/a/notes.txt", "not go")
	// Everything below must be skipped; the files do not even parse.
	write("testdata/x.go", "not go at all")
	write("internal/a/testdata/x.go", "not go at all")
	write(".hidden/x.go", "not go at all")
	write("_ignored/x.go", "not go at all")
	write(".git/x.go", "not go at all")
	write("nested/go.mod", "module example.com/nested\n")
	write("nested/n.go", "not go at all")

	files, err := loadModule(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		got = append(got, fmt.Sprintf("%s dir=%q test=%v", f.Name, f.Dir, f.Test))
	}
	want := []string{
		`internal/a/a.go dir="internal/a" test=false`,
		`internal/a/a_test.go dir="internal/a" test=true`,
		`main.go dir="" test=false`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("loaded files = %q, want %q", got, want)
	}

	write("internal/a/broken.go", "package a\nfunc {")
	if _, err := loadModule(root); err == nil {
		t.Error("loadModule accepted a file with a syntax error")
	}
}

func TestMatrixIsConsistent(t *testing.T) {
	for pkg, deps := range dependencyMatrix {
		if !slices.Contains(managedTrees, strings.SplitN(pkg, "/", 2)[0]) {
			t.Errorf("%s is outside the managed trees %v", pkg, managedTrees)
		}
		for _, dep := range deps {
			if _, ok := dependencyMatrix[dep]; !ok {
				t.Errorf("%s allows %s, which is not in the matrix", pkg, dep)
			}
			if dep == pkg {
				t.Errorf("%s allows itself", pkg)
			}
		}
	}
	// The matrix has no import cycle.
	const (
		visiting = 1
		done     = 2
	)
	state := make(map[string]int)
	var visit func(pkg string, trail []string)
	visit = func(pkg string, trail []string) {
		switch state[pkg] {
		case visiting:
			t.Errorf("dependency cycle: %s -> %s", strings.Join(trail, " -> "), pkg)
			return
		case done:
			return
		}
		state[pkg] = visiting
		for _, dep := range dependencyMatrix[pkg] {
			visit(dep, append(trail, pkg))
		}
		state[pkg] = done
	}
	for pkg := range dependencyMatrix {
		visit(pkg, nil)
	}
}

func TestMatrixLetsOnlyTheEngineImportStorage(t *testing.T) {
	for pkg, deps := range dependencyMatrix {
		if pkg != "internal/engine" && slices.Contains(deps, "internal/storage") {
			t.Errorf("%s may import internal/storage; only internal/engine may (R-FSM-03)", pkg)
		}
	}
	if !slices.Contains(dependencyMatrix["internal/engine"], "internal/storage") {
		t.Error("the engine must be able to import internal/storage")
	}
}

func TestPureAndTestOnlyPackagesAreInTheMatrix(t *testing.T) {
	for _, pkg := range purePackages {
		if _, ok := dependencyMatrix[pkg]; !ok {
			t.Errorf("pure package %s is not in the matrix", pkg)
		}
		// A pure package depends only on other pure packages.
		for _, dep := range dependencyMatrix[pkg] {
			if !slices.Contains(purePackages, dep) {
				t.Errorf("pure package %s depends on %s, which is not pure", pkg, dep)
			}
		}
	}
	for _, pkg := range testOnlyPackages {
		for other, deps := range dependencyMatrix {
			for _, dep := range deps {
				if inTree(dep, pkg) {
					t.Errorf("%s lists the test-only package %s as an allowed import", other, dep)
				}
			}
		}
	}
}
