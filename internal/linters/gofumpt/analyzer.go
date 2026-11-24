// Package gofumpt defines an analyzer that reports Go source files not
// formatted with gofumpt (a strict superset of gofmt).
//
// All gofumpt.Options fields are exposed as flags automatically via reflection.
// If gofumpt adds new options, they become available without code changes.
package gofumpt

import (
	"bytes"
	"flag"
	"os"
	"reflect"
	"strings"

	"golang.org/x/tools/go/analysis"
	"mvdan.cc/gofumpt/format"
)

var Analyzer = &analysis.Analyzer{
	Name:  "gofumpt",
	Doc:   "reports Go source files not formatted with gofumpt",
	Run:   run,
	Flags: buildFlags(),
}

type optionFlag struct {
	flag.Value
	value reflect.Value
}

func (f *optionFlag) String() string {
	if f == nil || f.Value == nil {
		return ""
	}
	return f.Value.String()
}

func (f *optionFlag) IsBoolFlag() bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func (f *optionFlag) Get() any {
	return f.value.Elem().Interface()
}

// buildFlags generates flags from format.Options struct fields via reflection.
func buildFlags() flag.FlagSet {
	var fs flag.FlagSet
	t := reflect.TypeOf(format.Options{})
	for i := range t.NumField() {
		f := t.Field(i)
		name := strings.ToLower(f.Name)
		value := reflect.New(f.Type)
		if fv, ok := value.Interface().(flag.Value); ok {
			fs.Var(&optionFlag{Value: fv, value: value}, name, f.Name+" option")
			continue
		}
		switch f.Type.Kind() {
		case reflect.String:
			fs.String(name, "", f.Name+" option")
		case reflect.Bool:
			fs.Bool(name, false, f.Name+" option")
		}
	}
	return fs
}

// optsFromFlags populates format.Options from flag values via reflection.
func optsFromFlags(fs *flag.FlagSet) format.Options {
	var opts format.Options
	v := reflect.ValueOf(&opts).Elem()
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		name := strings.ToLower(f.Name)
		fl := fs.Lookup(name)
		if fl == nil {
			continue
		}
		if getter, ok := fl.Value.(flag.Getter); ok {
			v.Field(i).Set(reflect.ValueOf(getter.Get()))
		}
	}
	return opts
}

func run(pass *analysis.Pass) (any, error) {
	opts := optsFromFlags(&pass.Analyzer.Flags)

	for _, file := range pass.Files {
		pos := pass.Fset.Position(file.Pos())
		if pos.Filename == "" {
			continue
		}

		src, err := os.ReadFile(pos.Filename)
		if err != nil {
			continue
		}

		formatted, err := format.Source(src, opts)
		if err != nil {
			continue
		}

		if !bytes.Equal(src, formatted) {
			pass.Report(analysis.Diagnostic{
				Pos:     file.Pos(),
				Message: "file is not gofumpt'd",
			})
		}
	}
	return nil, nil
}
