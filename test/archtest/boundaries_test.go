package archtest

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const module = "github.com/bibi45c/gateflow"

// area names the module a package belongs to: "services/<name>" or "test/<name>"
// for services and test tools, otherwise its top-level directory.
func area(importPath string) string {
	parts := strings.Split(strings.TrimPrefix(importPath, module+"/"), "/")
	if (parts[0] == "services" || parts[0] == "test") && len(parts) > 1 {
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

// violations lists imports that cross a module boundary: anything may import pkg/,
// and a package may import its own area; nothing else inside this module.
func violations(imports map[string][]string) []string {
	var found []string
	for pkg, deps := range imports {
		for _, dep := range deps {
			if !strings.HasPrefix(dep, module+"/") {
				continue
			}
			if target := area(dep); target != "pkg" && target != area(pkg) {
				found = append(found, pkg+" imports "+dep)
			}
		}
	}
	slices.Sort(found)
	return found
}

func TestModulesRespectBoundaries(t *testing.T) {
	format := `{{.ImportPath}} {{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}`
	out, err := exec.Command("go", "list", "-f", format, module+"/...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	imports := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		imports[fields[0]] = fields[1:]
	}
	if found := violations(imports); len(found) > 0 {
		t.Errorf("module boundary violations:\n%s", strings.Join(found, "\n"))
	}
}

func TestViolationsDetectsCrossServiceImports(t *testing.T) {
	imports := map[string][]string{
		module + "/services/gateway":                {module + "/pkg/server", module + "/services/gateway/internal/relay"},
		module + "/services/gateway/internal/relay": {module + "/services/metering/internal/ledger", "net/http"},
		module + "/pkg/server":                      {module + "/services/access"},
		module + "/test/mock-upstream":              {module + "/pkg/server", module + "/test/archtest"},
	}
	want := []string{
		module + "/pkg/server imports " + module + "/services/access",
		module + "/services/gateway/internal/relay imports " + module + "/services/metering/internal/ledger",
		module + "/test/mock-upstream imports " + module + "/test/archtest",
	}
	if got := violations(imports); !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
