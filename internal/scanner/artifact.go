package scanner

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// controllerSuffix is the trailing token on a controller repository directory
// name (for example "acm-controller"). It is stripped to derive the controller
// alias ("acm") used on the command line and in output.
const controllerSuffix = "-controller"

// generatorFileName is the code-generator configuration file at a controller
// repository root. Its presence is what distinguishes a valid controller
// checkout from any other directory under the workspace root: work-in-progress
// controllers that have not yet been scaffolded lack it and are skipped.
const generatorFileName = "generator.yaml"

// controllerRef identifies one discovered controller repository.
type controllerRef struct {
	// Alias is the controller name used on the command line ("acm").
	Alias string
	// Path is the absolute path to the controller repository checkout.
	Path string
}

// discoverControllers lists the controller repositories directly under root: an
// immediate subdirectory is a valid controller when it contains a
// generator.yaml. Work-in-progress checkouts that have not yet been scaffolded
// lack a generator.yaml and are skipped, so "scan all" only visits controllers
// it can actually scan. The result is sorted by alias so "scan all controllers"
// is deterministic.
//
// A non-existent root is treated as empty (nil slice, nil error), mirroring
// workspace.Discover, so callers can report "no controllers" rather than fail.
func discoverControllers(root string) ([]controllerRef, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var refs []controllerRef
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name())
		if !isValidControllerWorkspace(path) {
			continue
		}
		refs = append(refs, controllerRef{Alias: controllerAlias(e.Name()), Path: path})
	}
	sort.Slice(refs, func(a, b int) bool { return refs[a].Alias < refs[b].Alias })
	return refs, nil
}

// findController resolves a single controller by alias under root. The supplied
// identifier may be the bare alias ("acm") or the full directory name
// ("acm-controller"); both resolve to the same repository.
//
// The error distinguishes two cases so an explicit scan is actionable: a
// directory matching the alias that exists but lacks a generator.yaml (a
// work-in-progress checkout that has not been scaffolded yet) reports that it is
// not scannable, while a truly absent controller reports "not found".
func findController(root, identifier string) (controllerRef, error) {
	want := controllerAlias(identifier)
	refs, err := discoverControllers(root)
	if err != nil {
		return controllerRef{}, err
	}
	for _, r := range refs {
		if r.Alias == want {
			return r, nil
		}
	}
	if dir, ok := locateControllerDir(root, want); ok {
		return controllerRef{}, fmt.Errorf(
			"controller %q at %s is not scannable yet: no %s found (the controller may be a work in progress)",
			identifier, dir, generatorFileName)
	}
	return controllerRef{}, fmt.Errorf("controller %q not found under %s", identifier, root)
}

// locateControllerDir returns the path of an immediate subdirectory of root
// whose controller alias matches want, regardless of whether it is a valid
// controller workspace. It lets findController tell a work-in-progress checkout
// (present but not scaffolded) apart from a genuinely absent controller.
func locateControllerDir(root, want string) (string, bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() && controllerAlias(e.Name()) == want {
			return filepath.Join(root, e.Name()), true
		}
	}
	return "", false
}

// isValidControllerWorkspace reports whether the directory at repoPath is a
// scannable controller checkout. The presence of a generator.yaml (the
// code-generator configuration) is what distinguishes a controller from any
// other directory under the workspace root; work-in-progress controllers that
// have not yet been scaffolded lack it and are treated as not scannable.
func isValidControllerWorkspace(repoPath string) bool {
	_, err := os.Stat(filepath.Join(repoPath, generatorFileName))
	return err == nil
}

// controllerAlias derives the controller alias from a directory or identifier by
// removing the conventional "-controller" suffix.
func controllerAlias(name string) string {
	return strings.TrimSuffix(name, controllerSuffix)
}

// generatorConfig is the subset of a controller's generator.yaml the scanner
// reads: the set of resource Kinds it manages. The per-resource configuration
// itself is inspected by the agent with run_command, so only the resource keys
// are decoded here.
type generatorConfig struct {
	Resources map[string]resourceConfig `yaml:"resources"`
}

// resourceConfig is intentionally empty: only the presence of a resource key
// under `resources:` matters for enumeration.
type resourceConfig struct{}

// loadGeneratorConfig reads and decodes the controller's generator.yaml.
func loadGeneratorConfig(repoPath string) (generatorConfig, error) {
	data, err := os.ReadFile(filepath.Join(repoPath, generatorFileName))
	if err != nil {
		return generatorConfig{}, fmt.Errorf("reading generator config: %w", err)
	}
	var cfg generatorConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return generatorConfig{}, fmt.Errorf("parsing generator config: %w", err)
	}
	return cfg, nil
}

// findResourceCRD returns the contents of the CustomResourceDefinition manifest
// under helm/crds whose Kind matches resource. Matching by Kind (rather than by
// filename) is robust to the varied CRD file naming across controllers.
func findResourceCRD(repoPath, resource string) (string, error) {
	dir := filepath.Join(repoPath, "helm", "crds")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("reading CRD directory: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !(strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml")) {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, e.Name()))
		if readErr != nil {
			continue
		}
		var doc struct {
			Spec struct {
				Names struct {
					Kind string `yaml:"kind"`
				} `yaml:"names"`
			} `yaml:"spec"`
		}
		if yaml.Unmarshal(data, &doc) == nil && strings.EqualFold(doc.Spec.Names.Kind, resource) {
			return string(data), nil
		}
	}
	return "", fmt.Errorf("no CRD found for resource %q under %s", resource, dir)
}

// discoverResources returns the resource Kinds declared under the controller's
// generator.yaml `resources:` map, sorted, so "scan all resources" is
// deterministic.
func discoverResources(repoPath string) ([]string, error) {
	cfg, err := loadGeneratorConfig(repoPath)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(cfg.Resources))
	for kind := range cfg.Resources {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out, nil
}
