/* Copyright 2018 The Bazel Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package proto

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/rule"
)

func (_ *protoLang) GenerateRules(args language.GenerateArgs) (empty, gen []*rule.Rule) {
	c := args.Config
	pc := GetProtoConfig(c)
	if !pc.Mode.ShouldGenerateRules() {
		// Don't create or delete proto rules in this mode. Any existing rules
		// are likely hand-written.
		return nil, nil
	}

	g := newGenerator(args)
	pkgs := g.buildPackages(pc, args.Dir)
	for _, pkg := range pkgs {
		r := g.generateProto(pc, pkg)
		if r.IsEmpty(protoKinds[r.Kind()]) {
			empty = append(empty, r)
		} else {
			gen = append(gen, r)
		}
	}
	sort.SliceStable(gen, func(i, j int) bool {
		return gen[i].Name() < gen[j].Name()
	})
	empty = append(empty, g.generateEmpty()...)
	return empty, gen
}

type generator struct {
	c                   *config.Config
	file                *rule.File
	rel                 string
	shouldSetVisibility bool
	regularProtoFiles   []string
	genProtoFiles       []string
}

func newGenerator(args language.GenerateArgs) *generator {
	var regularProtoFiles []string
	for _, name := range args.RegularFiles {
		if strings.HasSuffix(name, ".proto") {
			regularProtoFiles = append(regularProtoFiles, name)
		}
	}
	var genProtoFiles []string
	for _, name := range args.GenFiles {
		if strings.HasSuffix(name, ".proto") {
			genProtoFiles = append(args.GenFiles, name)
		}
	}
	shouldSetVisibility := args.File == nil || !hasDefaultVisibility(args.File)
	return &generator{
		c:                   args.Config,
		file:                args.File,
		rel:                 args.Rel,
		shouldSetVisibility: shouldSetVisibility,
		regularProtoFiles:   regularProtoFiles,
		genProtoFiles:       genProtoFiles,
	}
}

// RuleName returns a name for a proto_library derived from the given strings.
// For each string, RuleName will look for a non-empty suffix of identifier
// characters and then append "_proto" to that.
func RuleName(names ...string) string {
	base := "root"
	for _, name := range names {
		notIdent := func(c rune) bool {
			return !('A' <= c && c <= 'Z' ||
				'a' <= c && c <= 'z' ||
				'0' <= c && c <= '9' ||
				c == '_')
		}
		if i := strings.LastIndexFunc(name, notIdent); i >= 0 {
			name = name[i+1:]
		}
		if name != "" {
			base = name
			break
		}
	}
	return base + "_proto"
}

// buildPackage extracts metadata from the .proto files in a directory and
// constructs possibly several packages, then selects a package to generate
// a proto_library rule for.
func (g *generator) buildPackages(pc *ProtoConfig, dir string) []*Package {
	packageMap := make(map[string]*Package)
	for _, name := range g.regularProtoFiles {
		info := protoFileInfo(dir, name)
		key := info.PackageName
		if pc.groupOption != "" {
			for _, opt := range info.Options {
				if opt.Key == pc.groupOption {
					key = opt.Value
					break
				}
			}
		}
		if packageMap[key] == nil {
			packageMap[key] = newPackage(info.PackageName)
		}
		packageMap[key].addFile(info)
	}

	switch pc.Mode {
	case DefaultMode:
		pkg, err := selectPackage(dir, g.rel, packageMap)
		if err != nil {
			log.Print(err)
		}
		if pkg == nil {
			return nil // empty rule created in generateEmpty
		}
		for _, name := range g.genProtoFiles {
			pkg.addGenFile(dir, name)
		}
		return []*Package{pkg}

	case PackageMode:
		pkgs := make([]*Package, 0, len(packageMap))
		for _, pkg := range packageMap {
			pkgs = append(pkgs, pkg)
		}
		return pkgs

	default:
		return nil
	}
}

// selectPackage chooses a package to generate rules for.
func selectPackage(dir, rel string, packageMap map[string]*Package) (*Package, error) {
	if len(packageMap) == 0 {
		return nil, nil
	}
	if len(packageMap) == 1 {
		for _, pkg := range packageMap {
			return pkg, nil
		}
	}
	defaultPackageName := strings.Replace(rel, "/", "_", -1)
	for _, pkg := range packageMap {
		if pkgName := goPackageName(pkg); pkgName != "" && pkgName == defaultPackageName {
			return pkg, nil
		}
	}
	return nil, fmt.Errorf("%s: directory contains multiple proto packages. Gazelle can only generate a proto_library for one package.", dir)
}

// goPackageName guesses the identifier in package declarations at the top of
// the .pb.go files that will be generated for this package. "" is returned
// if the package name cannot be determined.
//
// TODO(jayconrod): remove all Go-specific functionality. This is here
// temporarily for compatibility.
func goPackageName(pkg *Package) string {
	if opt, ok := pkg.Options["go_package"]; ok {
		if i := strings.IndexByte(opt, ';'); i >= 0 {
			return opt[i+1:]
		} else if i := strings.LastIndexByte(opt, '/'); i >= 0 {
			return opt[i+1:]
		} else {
			return opt
		}
	}
	if pkg.Name != "" {
		return strings.Replace(pkg.Name, ".", "_", -1)
	}
	if len(pkg.Files) == 1 {
		for s := range pkg.Files {
			return strings.TrimSuffix(s, ".proto")
		}
	}
	return ""
}

// generateProto creates a new proto_library rule for a package. The rule may
// be empty if there are no sources.
func (g *generator) generateProto(pc *ProtoConfig, pkg *Package) *rule.Rule {
	var name string
	if pc.Mode == DefaultMode {
		name = RuleName(goPackageName(pkg), pc.GoPrefix, g.rel)
	} else {
		name = RuleName(pkg.Options[pc.groupOption], pkg.Name, g.rel)
	}
	r := g.newRule("proto_library", name)
	srcs := make([]string, 0, len(pkg.Files))
	for f := range pkg.Files {
		srcs = append(srcs, f)
	}
	sort.Strings(srcs)
	if len(srcs) > 0 {
		r.SetAttr("srcs", srcs)
	}
	r.SetPrivateAttr(PackageKey, *pkg)
	imports := make([]string, 0, len(pkg.Imports))
	for i := range pkg.Imports {
		imports = append(imports, i)
	}
	sort.Strings(imports)
	r.SetPrivateAttr(config.GazelleImportsKey, imports)
	for k, v := range pkg.Options {
		r.SetPrivateAttr(k, v)
	}
	if g.shouldSetVisibility {
		vis := checkInternalVisibility(g.rel, "//visibility:public")
		r.SetAttr("visibility", []string{vis})
	}
	return r
}

// generateEmpty generates a list of proto_library rules that may be deleted.
// This is generated from existing proto_library rules with srcs lists that
// don't match any static or generated files.
func (g *generator) generateEmpty() []*rule.Rule {
	if g.file == nil {
		return nil
	}
	knownFiles := make(map[string]bool)
	for _, f := range g.regularProtoFiles {
		knownFiles[f] = true
	}
	for _, f := range g.genProtoFiles {
		knownFiles[f] = true
	}
	var empty []*rule.Rule
outer:
	for _, r := range g.file.Rules {
		if r.Kind() != g.c.MapKind("proto_library") {
			continue
		}
		srcs := r.AttrStrings("srcs")
		if len(srcs) == 0 && r.Attr("srcs") != nil {
			// srcs is not a string list; leave it alone
			continue
		}
		for _, src := range r.AttrStrings("srcs") {
			if knownFiles[src] {
				continue outer
			}
		}
		empty = append(empty, g.newRule("proto_library", r.Name()))
	}
	return empty
}

// hasDefaultVisibility returns whether oldFile contains a "package" rule with
// a "default_visibility" attribute. Rules generated by Gazelle should not
// have their own visibility attributes if this is the case.
func hasDefaultVisibility(f *rule.File) bool {
	if f == nil {
		return false
	}
	for _, r := range f.Rules {
		if r.Kind() == "package" && r.Attr("default_visibility") != nil {
			return true
		}
	}
	return false
}

// checkInternalVisibility overrides the given visibility if the package is
// internal.
func checkInternalVisibility(rel, visibility string) string {
	if i := strings.LastIndex(rel, "/internal/"); i >= 0 {
		visibility = fmt.Sprintf("//%s:__subpackages__", rel[:i])
	} else if strings.HasPrefix(rel, "internal/") {
		visibility = "//:__subpackages__"
	}
	return visibility
}

// newRule returns a new rule, applying any configured kind mappings.
func (g *generator) newRule(ruleKind, ruleName string) *rule.Rule {
	return rule.NewRule(g.c.MapKind(ruleKind), ruleName)
}
