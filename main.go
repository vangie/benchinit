// Copyright (c) 2018, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package main

import (
	"flag"
	"fmt"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"

	"golang.org/x/tools/go/packages"
)

var (
	recursive = flagSet.Bool("r", false, "include inits of transitive dependencies")
)

func init() {
	flagSet.Usage = usage
}

func main() {
	os.Exit(main1())
}

func main1() int {
	testflags, rest := lazyFlagParse(os.Args[1:])
	if err := flagSet.Parse(rest); err != nil {
		if err != flag.ErrHelp {
			fmt.Fprintf(os.Stderr, "flag: %v\n", err)
			usage()
		}
		return 2
	}

	cfg := &packages.Config{Mode: packages.LoadAllSyntax}
	pkgs, err := packages.Load(cfg, flagSet.Args()...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load: %v\n", err)
		return 1
	}
	if packages.PrintErrors(pkgs) > 0 {
		return 1
	}

	for _, pkg := range pkgs {
		cleanup, err := setup(pkg)
		defer cleanup()
		if err != nil {
			fmt.Fprintf(os.Stderr, "benchmark: %v\n", err)
			return 1
		}
	}

	if err := benchmark(pkgs, testflags); err != nil {
		return 1
	}
	return 0
}

// testFlag is copied from cmd/go/internal/test/testflag.go's testFlagDefn, with
// small modifications.
var testFlagDefn = []struct {
	Name    string
	BoolVar bool
}{
	// local.
	{Name: "c", BoolVar: true},
	{Name: "i", BoolVar: true},
	{Name: "o"},
	{Name: "cover", BoolVar: true},
	{Name: "covermode"},
	{Name: "coverpkg"},
	{Name: "exec"},
	{Name: "json", BoolVar: true},
	{Name: "vet"},

	// Passed to 6.out, adding a "test." prefix to the name if necessary: -v becomes -test.v.
	{Name: "bench"},
	{Name: "benchmem", BoolVar: true},
	{Name: "benchtime"},
	{Name: "blockprofile"},
	{Name: "blockprofilerate"},
	{Name: "count"},
	{Name: "coverprofile"},
	{Name: "cpu"},
	{Name: "cpuprofile"},
	{Name: "failfast", BoolVar: true},
	{Name: "list"},
	{Name: "memprofile"},
	{Name: "memprofilerate"},
	{Name: "mutexprofile"},
	{Name: "mutexprofilefraction"},
	{Name: "outputdir"},
	{Name: "parallel"},
	{Name: "run"},
	{Name: "short", BoolVar: true},
	{Name: "timeout"},
	{Name: "trace"},
	{Name: "v", BoolVar: true},
}

var flagSet = flag.NewFlagSet("benchinit", flag.ContinueOnError)

func usage() {
	fmt.Fprintf(os.Stderr, `
Usage of benchinit:

	benchinit [benchinit flags] [go test flags] [packages]

For example:

	benchinit -count=10 .

All flags accepted by 'go test', including the benchmarking ones, should be
accepted. See 'go help testflag' for a complete list.
`[1:])
}

// lazyFlagParse is similar to flag.Parse, but keeps 'go test' flags around so
// they can be passed on. We'll add our own benchinit flags at a later time.
func lazyFlagParse(args []string) (testflags, rest []string) {
_args:
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "" || arg == "--" || arg[0] != '-' {
			rest = append(rest, args[i:]...)
			break
		}
		for _, tflag := range testFlagDefn {
			if arg[1:] == tflag.Name {
				if tflag.BoolVar {
					// e.g. -benchmem
					testflags = append(testflags, arg)
					continue _args
				}
				next := ""
				if i+1 < len(args) {
					i++
					next = args[i]
				}
				testflags = append(testflags, arg, next)
				continue _args
			} else if strings.HasPrefix(arg[1:], tflag.Name+"=") {
				// e.g. -count=10
				testflags = append(testflags, arg)
				continue _args
			}
		}
		// Likely one of our flags. Leave it to flagSet.Parse.
		rest = append(rest, arg)
	}
	return testflags, rest
}

const (
	benchFile = "benchinit_generated_test.go"
	stubFile  = "benchinit_generated_stub.go"
)

var sizes = types.SizesFor("gc", runtime.GOARCH)

func setup(pkg *packages.Package) (cleanup func(), _ error) {
	if len(pkg.GoFiles) == 0 {
		// No non-test Go files; no init work to benchmark. Do nothing,
		// and the 'go test -bench' command later will do little work
		// here.
		return func() {}, nil
	}

	var toDelete []string

	cleanup = func() {
		for _, path := range toDelete {
			if err := os.Remove(path); err != nil {
				// TODO: return the error instead? how likely is
				// it to happen?
				panic(err)
			}
		}
	}

	// Place the benchmark file in the same package, to ensure that we can
	// also benchmark transitive internal dependencies.
	// We assume 'go list' packages; all package files in the same directory.
	// TODO: since we use go/packages, add support for other build systems
	// and test it.
	dir := filepath.Dir(pkg.GoFiles[0])

	data := tmplData{
		Package: pkg,
	}
	roots := []*packages.Package{pkg}
	packages.Visit(roots, func(pkg *packages.Package) bool {
		switch pkg.PkgPath {
		case "runtime", // messes up everything
			"testing",   // messes up the benchmark itself
			"os/signal", // messes up signal.Notify
			"time":      // messes up monotonic times
			// skip their imports as well.
			return false
		}
		data.Inits = append(data.Inits, pkg.PkgPath)
		if !*recursive {
			// not in recursive mode.
			return false
		}
		return true
	}, nil)

	scope := pkg.Types.Scope()
	for _, name := range scope.Names() {
		v, ok := scope.Lookup(name).(*types.Var)
		if !ok {
			continue
		}
		switch t := v.Type(); t.String() {
		case "flag.FlagSet":
			st := t.Underlying().(*types.Struct)
			var fields []*types.Var
			var field *types.Var
			index := -1
			for i := 0; i < st.NumFields(); i++ {
				field = st.Field(i)
				if field.Name() == "formal" {
					index = i
					// continue so that we grab all fields
				}
				fields = append(fields, field)
			}
			if index == -1 {
				panic("field not found")
			}
			offsets := sizes.Offsetsof(fields)
			data.ToZero = append(data.ToZero, toZero{
				PkgPath:   pkg.PkgPath,
				Name:      name,
				TotalSize: sizes.Sizeof(t),
				Offset:    offsets[index],
				ZeroSize:  sizes.Sizeof(field.Type()),
			})
		}

	}

	bench := filepath.Join(dir, benchFile)
	if err := templateFile(bench, benchTmpl, data); err != nil {
		return cleanup, err
	}
	toDelete = append(toDelete, bench)

	stub := filepath.Join(dir, stubFile)
	if err := templateFile(stub, stubTmpl, data); err != nil {
		return cleanup, err
	}
	toDelete = append(toDelete, stub)
	return cleanup, nil
}

func benchmark(pkgs []*packages.Package, testflags []string) error {
	args := []string{"test",
		"-run=^$",                // disable all tests
		"-vet=off",               // disable vet
		"-bench=^BenchmarkInit$", // only run the one benchmark
	}
	args = append(args, testflags...) // add the user's test args

	for _, pkg := range pkgs {
		args = append(args, pkg.PkgPath)
	}

	cmd := exec.Command("go", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// templateFile creates a file at path and fills its contents with the
// execution of the template with some data. It errors if the file exists or
// cannot be created.
func templateFile(path string, tmpl *template.Template, data interface{}) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := tmpl.Execute(f, data); err != nil {
		f.Close()
		return err
	}
	return f.Close() // check for an error
}

type tmplData struct {
	// Package is the non-test package being benchmarked.
	*packages.Package
	// Inits are the package paths of all the init functions to be
	// benchmarked. By default, this will only contain the import path of
	// the package itself. In recursive mode (-r), it will include the
	// import paths of transitive dependencies too, excluding the ones whose
	// initdone we can't mess with.
	Inits []string

	ToZero []toZero
}

type toZero struct {
	PkgPath, Name string

	TotalSize        int64
	Offset, ZeroSize int64
}

var benchTmpl = template.Must(template.New("").Parse(`
// Code generated by benchinit. DO NOT EDIT.

package {{.Name}}_test

import (
	"testing"
	_ "unsafe" // must import unsafe to use go:linkname
)

func BenchmarkInit(b *testing.B) {
	// Allocs tend to matter too, and have no downsides.
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		b.StartTimer()
		_init()
		b.StopTimer()

		deinit() // get ready to run init again
	}
}

// deinit undoes the work that the init functions being benchmarked do. In
// particular, "initdone" is set to 0 to get init to do work again, and any
// globals known to cause issues are reset.
func deinit() {
	{{- range $i, $g := .ToZero }}
	for i := {{$g.Offset}}; i < {{$g.Offset}}+{{$g.ZeroSize}}; i++ {
		_tozero{{$i}}[i] = 0
	}
	{{- end }}

	{{- range $i, $_ := .Inits }}
	_initdone{{$i}} = 0
	{{- end }}
}

//go:linkname _init {{.PkgPath}}.init
func _init()

{{ range $i, $g := .ToZero }}
//go:linkname _tozero{{$i}} {{$g.PkgPath}}.{{$g.Name}}
var _tozero{{$i}} [{{$g.TotalSize}}]byte
{{- end }}

{{- range $i, $path := .Inits }}
//go:linkname _initdone{{$i}} {{$path}}.initdone·
var _initdone{{$i}} uint8
{{- end }}
`[1:]))

var stubTmpl = template.Must(template.New("").Parse(`
// Code generated by benchinit. DO NOT EDIT.

package {{.Name}}

func init() {}
`[1:]))
