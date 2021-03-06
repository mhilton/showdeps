package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/build"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/kisielk/gotool"
)

var (
	noTestDeps = flag.Bool("T", false, "exclude test dependencies")
	all        = flag.Bool("a", false, "show all dependencies recursively (only test dependencies from the root packages are shown)")
	std        = flag.Bool("stdlib", false, "show stdlib dependencies")
	from       = flag.Bool("from", false, "show which dependencies are introduced by which packages")
	why        = flag.String("why", "", "show only packages which import directly or indirectly the specified package (implies -a and -from)")
	files      = flag.Bool("f", false, "list Go source files instead of packages (overrides -from and -why)")
)

var whyMatch func(string) bool

var helpMessage = `
usage: showdeps [flags] [pkg....]

showdeps prints Go package dependencies of the named packages, specified
as in the Go command (for instance ... wildcards work), one per line.
If no packages are given, it uses the package in the current directory.

Note that testing dependencies are only considered if they are
in the packages specified on the command line. That is testing
dependencies are not considered transitively.

By default it prints direct dependencies of the packages (and their tests)
only, but the -a flag can be used to print all reachable dependencies.

If the -from flag is specified, the package path on each line is followed
by the paths of all the packages that depend on it.

If the package argument to the -why flag is in the standard library,
the -std flag is implied. The -why flag can also specify Go-command-style
... wildcards.

If the -f flag is provided, instead of packages, showdeps will print
all the Go source files in the package. It also includes the
source of the packages specified directly on the command line,
including their test files unless the -T flag is provided.
`[1:]

var cwd string

func main() {
	flag.Usage = func() {
		os.Stderr.WriteString(helpMessage)
		flag.PrintDefaults()
		os.Exit(2)
	}
	flag.Parse()
	pkgs := flag.Args()
	if len(pkgs) == 0 {
		pkgs = []string{"."}
	}
	if d, err := os.Getwd(); err != nil {
		log.Fatalf("cannot get working directory: %v", err)
	} else {
		cwd = d
	}
	if *why != "" {
		*all = true
		*from = true
		if isStdlib(*why) {
			*std = true
		}
		whyMatch = matchPattern(*why)
	}

	pkgs = gotool.ImportPaths(pkgs)
	rootPkgs := make(map[string]bool)
	for _, pkg := range pkgs {
		p, err := build.Default.Import(pkg, cwd, build.FindOnly)
		if err != nil {
			log.Fatalf("cannot find %q: %v", pkg, err)
		}
		rootPkgs[p.ImportPath] = true
	}

	allPkgs := make(map[string][]string)
	for _, pkg := range pkgs {
		if err := findImports(pkg, allPkgs, rootPkgs); err != nil {
			log.Fatalf("cannot find imports from %q: %v", pkg, err)
		}
	}
	if !*files {
		// Delete packages specified directly on the command line.
		for pkg := range rootPkgs {
			delete(allPkgs, pkg)
		}
		if whyMatch != nil {
			// Delete all packages that don't directly or indirectly import *why.
			marked := make(map[string]bool)
			for pkg := range allPkgs {
				if whyMatch(pkg) {
					markImporters(pkg, allPkgs, marked)
				}
			}
			for pkg := range allPkgs {
				if !marked[pkg] {
					delete(allPkgs, pkg)
				}
			}
		}
	}

	result := make([]string, 0, len(allPkgs))
	for name := range allPkgs {
		result = append(result, name)
	}
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	sort.Strings(result)
	for _, r := range result {
		switch {
		case *files:
			pkg, _ := build.Default.Import(r, cwd, 0)
			showFiles(w, pkg, pkg.GoFiles)
			showFiles(w, pkg, pkg.CgoFiles)
			if rootPkgs[pkg.ImportPath] && !*noTestDeps {
				// It's a package specified directly on the command line.
				// Show its test files too.
				showFiles(w, pkg, pkg.TestGoFiles)
				showFiles(w, pkg, pkg.XTestGoFiles)
			}
		case *from:
			from := allPkgs[r]
			sort.Strings(from)
			from = uniq(from)
			fmt.Fprintf(w, "%s %s\n", r, strings.Join(from, " "))
		default:
			fmt.Fprintln(w, r)
		}
	}
}

func showFiles(w io.Writer, pkg *build.Package, fs []string) {
	for _, f := range fs {
		fmt.Fprintln(w, filepath.Join(pkg.Dir, f))
	}
}

func uniq(ss []string) []string {
	j := 0
	prev := ""
	for _, s := range ss {
		if s != prev {
			ss[j] = s
			j++
			prev = s
		}
	}
	return ss[0:j]
}

// markImporters sets a marked entry to true for every entry in allPkgs
// that is imported by pkg, including pkg itself.
func markImporters(pkg string, allPkgs map[string][]string, marked map[string]bool) {
	if marked[pkg] {
		return
	}
	marked[pkg] = true // prevent infinite recursion
	for _, imp := range allPkgs[pkg] {
		markImporters(imp, allPkgs, marked)
	}
}

func isStdlib(pkg string) bool {
	return !strings.Contains(strings.SplitN(pkg, "/", 2)[0], ".")
}

// findImports recursively adds all imported packages of given
// package (packageName) to allPkgs map.
func findImports(packageName string, allPkgs map[string][]string, rootPkgs map[string]bool) error {
	if packageName == "C" {
		return nil
	}
	pkg, err := build.Default.Import(packageName, cwd, 0)
	if err != nil {
		return fmt.Errorf("cannot find %q: %v", packageName, err)
	}
	allPkgs[pkg.ImportPath] = allPkgs[pkg.ImportPath] // ensure the package has an entry.
	for name := range imports(pkg, rootPkgs[pkg.ImportPath]) {
		if !*std && isStdlib(name) {
			continue
		}
		alreadyDone := allPkgs[name] != nil
		allPkgs[name] = append(allPkgs[name], pkg.ImportPath)
		if *all && !alreadyDone {
			if err := findImports(name, allPkgs, rootPkgs); err != nil {
				return err
			}
		}
	}
	return nil
}

func imports(pkg *build.Package, isRoot bool) map[string]bool {
	imps := make(map[string]bool)
	addPackages(imps, pkg.Imports)
	if isRoot && !*noTestDeps {
		addPackages(imps, pkg.TestImports)
		addPackages(imps, pkg.XTestImports)
	}
	return imps
}

func addPackages(m map[string]bool, ss []string) {
	for _, s := range ss {
		if *std || !isStdlib(s) {
			m[s] = true
		}
	}
}

// matchPattern(pattern)(name) reports whether
// name matches pattern.  Pattern is a limited glob
// pattern in which '...' means 'any string' and there
// is no other special syntax.
// Stolen from the go tool
func matchPattern(pattern string) func(name string) bool {
	re := regexp.QuoteMeta(pattern)
	re = strings.Replace(re, `\.\.\.`, `.*`, -1)
	// Special case: foo/... matches foo too.
	if strings.HasSuffix(re, `/.*`) {
		re = re[:len(re)-len(`/.*`)] + `(/.*)?`
	}
	reg := regexp.MustCompile(`^` + re + `$`)
	return func(name string) bool {
		return reg.MatchString(name)
	}
}
