package generator

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"strings"

	"github.com/pkg/errors"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"golang.org/x/tools/go/loader"
)

type Generator struct {
	Name      string
	Program   *loader.Program
	Package   *loader.PackageInfo
	Resources map[string]*schema.ResourceSpec
	Types     map[string]*schema.ComplexTypeSpec
}

func (g *Generator) Schema() *schema.PackageSpec {
	spec := schema.PackageSpec{
		Name: g.Name,
	}

	for k, v := range g.Resources {
		if spec.Resources == nil {
			spec.Resources = make(map[string]schema.ResourceSpec)
		}
		spec.Resources[g.defaultType(k)] = *v
	}
	for k, v := range g.Types {
		if spec.Types == nil {
			spec.Types = make(map[string]schema.ComplexTypeSpec)
		}
		spec.Types[g.defaultType(k)] = *v
	}

	return &spec
}

// GatherPackageSchema enumerates all package-scoped types, processes them, and
// generates the schema specs for any that are of the expected kind (resources, etc).
func (g *Generator) GatherPackageSchema() error {
	scope := g.Package.Pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		switch o := obj.(type) {
		case *types.TypeName:
			err := g.GatherTypeSchemas(o)
			if err != nil {
				return errors.Wrapf(err, "gathering Go type '%v'", name)
			}
		}
	}

	return nil
}

// getTypeSpec finds the parsed AST information for the given type. This provides
// us access to parser-only information such as comments.
func (g *Generator) getTypeNode(t *types.TypeName) (*ast.TypeSpec, error) {
	for _, file := range g.Package.Files {
		for _, decl := range file.Decls {
			if gdecl, isgdecl := decl.(*ast.GenDecl); isgdecl {
				for _, spec := range gdecl.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok {
						if ts.Name.Name == t.Name() {
							return ts, nil
						}
					}
				}
			}
		}
	}
	return nil, errors.Errorf("missing Go declaration for %v", t.Name())
}

func (g *Generator) GatherTypeSchemas(t *types.TypeName) error {
	// First look up the declaration for this type.
	node, err := g.getTypeNode(t)
	if err != nil {
		return errors.Wrapf(err, "gathering Go type info")
	}

	// Now check the members of the type and ensure that it's of the expected shape.
	switch typ := t.Type().(type) {
	case *types.Named:
		switch s := typ.Underlying().(type) {
		case *types.Struct:
			// A struct definition, possibly a resource.  First, check that all the fields are supported types.
			return g.gatherStructSchemas(node, t, s)
		default:
			return errors.Errorf("%s: %v is an illegal underlying type: %v", g.diag(node), s, reflect.TypeOf(s))
		}
	default:
		return errors.Errorf("%s: %v is an illegal Go type kind: %v", g.diag(node), t.Name(), reflect.TypeOf(typ))
	}
	return nil
}

func (g *Generator) gatherPropertySchemas(node *ast.TypeSpec, t *types.TypeName,
	s *types.Struct) (map[string]schema.PropertySpec, map[string]PropertyOptions, error) {

	// Now declare the output maps and walk the fields.
	isRes := IsResource(t, s)
	props := make(map[string]schema.PropertySpec)
	propOpts := make(map[string]PropertyOptions)
	for i := 0; i < s.NumFields(); i++ {
		// See if there is a Pulumi tag; if not, skip this field.
		has, opts, err := ParsePropertyOptions(s.Tag(i))
		if err != nil {
			return nil, nil, err
		} else if !has {
			continue
		}

		// Fetch the field and validate the options.
		fld := s.Field(i)
		if opts.Name == "" {
			return nil, nil, errors.Errorf("%s: field %v.%v is missing a `pulumi:\"<name>\"` tag directive",
				g.diag(fld), t.Name(), fld.Name())
		}
		if opts.Out && !isRes {
			return nil, nil, errors.Errorf("%s: field %v.%v is marked `out` but is not a resource property",
				g.diag(fld), t.Name(), fld.Name())
		}
		if opts.Replaces && !isRes {
			return nil, nil, errors.Errorf("%s: field %v.%v is marked `replaces` but is not a resource property",
				g.diag(fld), t.Name(), fld.Name())
		}
		if _, isPtr := fld.Type().(*types.Pointer); !isPtr && opts.Optional {
			return nil, nil, errors.Errorf("%s: field %v.%v is marked `optional` but is not a pointer in the schema",
				g.diag(fld), t.Name(), fld.Name())
		}

		// Generate the PropertySpec for this property based on its type.
		propType, err := g.gatherSchemaType(fld.Type(), opts)
		if err != nil {
			return nil, nil, errors.Errorf("%s: field %v.%v is an not a legal schema type: %v",
				g.diag(fld), t.Name(), fld.Name(), err)
		}
		propSpec := schema.PropertySpec{
			TypeSpec: *propType,
		}

		// Use the property's doc-comment as the description, if available.
		if structNode, ok := node.Type.(*ast.StructType); ok {
			if comment := structNode.Fields.List[i].Doc; comment != nil {
				propSpec.Description = cleanComment(comment.Text())
			}
		}

		// TODO: keep track of required/outs/etc, for returning.

		props[opts.Name] = propSpec
	}

	return props, propOpts, nil
}

// gatherStructSchemas interprets a Go struct declaration and deeply generates the resource, and/or plain old,
// types, depending on what contents are found within.
func (g *Generator) gatherStructSchemas(node *ast.TypeSpec, t *types.TypeName, s *types.Struct) error {
	// Skip those that we've already visited.
	name := t.Name()
	_, hasType := g.Types[name]
	_, hasResource := g.Resources[name]
	if hasType || hasResource {
		return nil
	}

	// Extract the property metadata.
	props, _, err := g.gatherPropertySchemas(node, t, s)
	if err != nil {
		return err
	}

	// Now generate the appropriate schema information based on what we've found.
	typeSpec := schema.ObjectTypeSpec{
		Type:       "object",
		Properties: props,
	}
	// TODO: description, required, based on tags

	// Use the type's doc-comment as the description, if available.
	if node.Doc != nil {
		typeSpec.Description = cleanComment(node.Doc.Text())
	}

	if IsResource(t, s) {
		g.Resources[name] = &schema.ResourceSpec{
			ObjectTypeSpec: typeSpec,
			IsComponent:    true,
		}
	} else if len(props) > 0 {
		g.Types[name] = &schema.ComplexTypeSpec{
			ObjectTypeSpec: typeSpec,
		}
	}

	return nil
}

// gatherSchemaType ensures that a type has been created for the target type, and returns
// a TypeSpec to it, either by name or reference, as appropriate.
func (g *Generator) gatherSchemaType(t types.Type, opts PropertyOptions) (*schema.TypeSpec, error) {
	// Only these types are legal:
	//     - Primitives: bool, int, float, string
	//     - Other structs
	//     - Pointers to any of the above
	//     - Pointers to other resource types
	//     - Arrays of the above things
	//     - Maps with string keys and any of the above as values
	switch ft := t.(type) {
	case *types.Basic:
		if basic, isbasic := t.(*types.Basic); isbasic {
			switch basic.Kind() {
			case types.Bool:
				return &schema.TypeSpec{Type: "boolean"}, nil
			case types.Int, types.Int16, types.Int32, types.Int64:
				return &schema.TypeSpec{Type: "integer"}, nil
			case types.Float32, types.Float64:
				return &schema.TypeSpec{Type: "number"}, nil
			case types.String:
				return &schema.TypeSpec{Type: "string"}, nil
			}
		}
		return nil, errors.Errorf("bad primitive type %v; must be bool, int, float, or string", ft)
	case *types.Interface:
		// interface{} is fine and is interpreted as any valid type. There is no "any" type
		// in JSON schema, instead, we simply leave the type empty which means "no constraints."
		// TODO: is this right? "object" is a map. Is Pulumi schema doing the right thing?
		return &schema.TypeSpec{Type: "object"}, nil
	case *types.Named:
		switch ut := ft.Underlying().(type) {
		case *types.Basic, *types.Interface:
			// A named type alias of another type, just recurse.
			return g.gatherSchemaType(ut, opts)
		case *types.Struct:
			// A struct can be either a reference to another struct within this package,
			// or a struct defined elsewhere. In either case, we emit a reference to it. For
			// structs defined within the same package, we don't visit the type, as it will
			// presumably be visited as a top-level declaration anyway. If something goes wrong
			// here, we'll generate a dangling ref, but the schema checker will catch that.
			refType := opts.Ref
			if refType == "" {
				refType = g.defaultRefType(ft.String())
			}
			return &schema.TypeSpec{Ref: refType}, nil
		default:
			return nil, errors.Errorf("bad named field type: %v", reflect.TypeOf(ut))
		}
	case *types.Pointer:
		// For pointers, just use the underlying type.
		// TODO: not sure exactly where pointers should be legal; for instance, should this imply optional?
		return g.gatherSchemaType(ft.Elem(), opts)
	case *types.Map:
		// A map is OK so long as its key is a string (or string-backed type) and its element type is legal.
		isStringKey := false
		switch kt := ft.Key().(type) {
		case *types.Basic:
			isStringKey = (kt.Kind() == types.String)
		case *types.Named:
			if bt, isbt := kt.Underlying().(*types.Basic); isbt {
				isStringKey = (bt.Kind() == types.String)
			}
		}
		if !isStringKey {
			return nil, errors.Errorf("map index type %v must be a string (or string-backed typedef)", ft.Key())
		}

		// Generate the element type and return the map type that references it.
		et, err := g.gatherSchemaType(ft.Elem(), opts)
		if err != nil {
			return nil, err
		}
		return &schema.TypeSpec{
			Type:                 "object",
			AdditionalProperties: et,
		}, nil
	case *types.Slice:
		// A slice is OK so long as its element type is also OK.
		et, err := g.gatherSchemaType(ft.Elem(), opts)
		if err != nil {
			return nil, err
		}
		return &schema.TypeSpec{
			Type:  "array",
			Items: et,
		}, nil
	}

	return nil, errors.Errorf("unrecognized field type %v: %v", t, reflect.TypeOf(t))
}

// defaultType generates a default fully qualified type name.
func (g *Generator) defaultType(t string) string {
	lix := strings.LastIndex(t, ".")
	if lix != -1 {
		t = t[lix+1:]
	}
	// TODO: support specifying the module.
	return fmt.Sprintf("%s:index:%s", g.Name, t)
}

// defaultRefType generates a default reference type. Unless otherwise noted, it assumes
// we are referencing another type within the same package.
func (g *Generator) defaultRefType(t string) string {
	dt := g.defaultType(t)
	return fmt.Sprintf("#/types/%s", dt)
}

// diag stringifies a Go element's position for purposes of diagnostics.
func (g *Generator) diag(elem goPos) string {
	pos := g.Program.Fset.Position(elem.Pos())
	return fmt.Sprintf("%s:%d,%d", pos.Filename, pos.Line, pos.Column)
}

type goPos interface {
	Pos() token.Pos
}

func cleanComment(s string) string {
	s = strings.Trim(s, "\n")            // get rid of trailing newline(s).
	s = strings.ReplaceAll(s, "\n", " ") // spaceify rather than multi-line comments.
	return s
}
