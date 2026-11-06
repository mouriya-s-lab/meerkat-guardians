package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exampleRoot copies config.example into a temp dir, points omp.extension at the repo's plugin
// and applies edits (file -> old/new string replacements).
func exampleRoot(t *testing.T, edits map[string][2]string) string {
	t.Helper()
	src, err := filepath.Abs("../../config.example")
	if err != nil {
		t.Fatal(err)
	}
	plugin, err := filepath.Abs("../../plugin/index.ts")
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	runtime := filepath.Join(dst, "guardians/runtime.yaml")
	b, err := os.ReadFile(runtime)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "extension:") {
			lines[i] = "  extension: " + plugin
		}
	}
	if err := os.WriteFile(runtime, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, e := range edits {
		p := filepath.Join(dst, rel)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), e[0]) {
			t.Fatalf("%s does not contain %q", rel, e[0])
		}
		if err := os.WriteFile(p, []byte(strings.Replace(string(b), e[0], e[1], 1)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func TestLoadExample(t *testing.T) {
	root := exampleRoot(t, nil)
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Bases) != 2 {
		t.Fatalf("bases = %+v", cfg.Bases)
	}
	b := cfg.Bases[0]
	if b.ID != "product-opus.oh-my-omp__pm-e2e-code.futureppo-pool__minimax-m3.1-flash" || b.Branch != "main" {
		t.Fatalf("base = %+v", b)
	}
	g, ok := cfg.Guardian("product-opus")
	if !ok {
		t.Fatal("guardian missing")
	}
	if g.ToolsArg() != "read,grep,glob,find,pm_base_ready,pm_return_facts" {
		t.Fatalf("tools = %s", g.ToolsArg())
	}
	if g.Prompt != filepath.Join(root, "guardians/omp/prompts/product-opus.md") {
		t.Fatalf("prompt = %s", g.Prompt)
	}
	opening, cont, err := cfg.Goals.Proxy.Render(ProxyVars{Seat: "S1", Agenda: "o/a#1", Repo: "o/r", Question: "Q?"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"S1", "o/a#1", "o/r", "Q?"} {
		if !strings.Contains(opening, want) {
			t.Fatalf("opening lacks %q: %s", want, opening)
		}
	}
	if !strings.Contains(cont, "pm_return_facts") {
		t.Fatalf("continue = %s", cont)
	}
	if !cfg.Repos.Readable("oh-my-omp/pm-e2e-design") || cfg.Repos.Readable("oh-my-omp/pm-e2e-agenda") {
		t.Fatal("Readable wrong")
	}
}

func TestLoadErrorsNameKeyPath(t *testing.T) {
	cases := []struct {
		name  string
		edits map[string][2]string
		want  string
	}{
		{
			"unknown key in list item",
			map[string][2]string{"guardians/bases.yaml": {"    branch: main\n", "    branch: main\n    brnach: dev\n"}},
			"guardians/bases.yaml: bases[0].brnach: unknown field",
		},
		{
			"unknown nested key",
			map[string][2]string{"guardians/runtime.yaml": {"  bin: omp\n", "  bin: omp\n  binary: omp\n"}},
			"guardians/runtime.yaml: omp.binary: unknown field",
		},
		{
			"missing required key",
			map[string][2]string{"guardians/bases.yaml": {"    branch: main\n", ""}},
			"guardians/bases.yaml: bases[0].branch: missing",
		},
		{
			"missing ready timeout",
			map[string][2]string{"guardians/runtime.yaml": {"  readyTimeoutSeconds: 120\n", ""}},
			"guardians/runtime.yaml: omp.readyTimeoutSeconds: missing",
		},
		{
			"zero exit grace",
			map[string][2]string{"guardians/runtime.yaml": {"exitGraceSeconds: 30", "exitGraceSeconds: 0"}},
			"guardians/runtime.yaml: omp.exitGraceSeconds: missing",
		},
		{
			"missing shared key",
			map[string][2]string{"shared/repos.yaml": {"omp: []\n", ""}},
			"shared/repos.yaml: omp: missing",
		},
		{
			"route tier without base",
			map[string][2]string{"guardians/routing.yaml": {"simple: futureppo-pool/minimax-m3.1-flash", "simple: openai/gpt-x"}},
			"guardians/routing.yaml: routes[0].tiers.simple: no base",
		},
		{
			"base repo not in repos.yaml",
			map[string][2]string{"guardians/bases.yaml": {"repo: oh-my-omp/pm-e2e-design", "repo: oh-my-omp/elsewhere"}},
			"guardians/bases.yaml: bases[1].repo: oh-my-omp/elsewhere is not listed",
		},
		{
			"unknown template field",
			map[string][2]string{"guardians/goals.yaml": {"{{.Question}}", "{{.Questoin}}"}},
			"guardians/goals.yaml: proxy.opening:",
		},
		{
			"wrong type",
			map[string][2]string{"guardians/runtime.yaml": {"maxConcurrentWarmups: 2", "maxConcurrentWarmups: two"}},
			"guardians/runtime.yaml: maxConcurrentWarmups:",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(exampleRoot(t, c.edits))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestBaseID(t *testing.T) {
	got := BaseID("product-opus", "owner/name", "ollama/qwen3:8b")
	if got != "product-opus.owner__name.ollama__qwen3_8b" {
		t.Fatal(got)
	}
}
