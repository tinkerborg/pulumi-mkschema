package main

import (
	"go/parser"

	"github.com/pkg/errors"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi-mkschema/pkg/generator"
	"golang.org/x/tools/go/loader"
)

// Generate loads the target package name, parses and analyzes it, and transforms it into
// a Pulumi package specification.
func Generate(puPkg, goPkg string) (*schema.PackageSpec, error) {
	// Now parse the files in the target package and get ready to analyze the contents.
	var conf loader.Config
	conf.ParserMode |= parser.ParseComments // retain Go doc comments, since we use them.
	if _, err := conf.FromArgs([]string{goPkg}, false); err != nil {
		return nil, errors.Wrapf(err, "loading Go parser")
	}
	prog, err := conf.Load()
	if err != nil {
		return nil, errors.Wrapf(err, "parsing Go files")
	}

	// Afterwards, find the specific package information for the root we parsed.
	var pkginfo *loader.PackageInfo
	for _, pkg := range prog.AllPackages {
		if pkg.Pkg.Path() == goPkg {
			pkginfo = pkg
			break
		}
	}

	// Create a checker context we'll use to populate the schema.
	g := &generator.Generator{
		Name:      puPkg,
		Program:   prog,
		Package:   pkginfo,
		Resources: make(map[string]*schema.ResourceSpec),
		Types:     make(map[string]*schema.ComplexTypeSpec),
	}

	// Analyze the AST and gather up all resource and schema types.
	if err = g.GatherPackageSchema(); err != nil {
		return nil, errors.Wrapf(err, "gathering Go package info")
	}

	return g.Schema(), nil
}
