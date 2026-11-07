package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const examplePrompt = "guardians/omp/prompts/product-opus.md"

// configFiles are the files a load reads under the configuration root: the yaml files and the prompt
// file the example guardian names.
var configFiles = []string{
	"guardians/runtime.yaml",
	"guardians/guardians.yaml",
	"guardians/bases.yaml",
	"guardians/routing.yaml",
	"guardians/goals.yaml",
	"shared/repos.yaml",
	"shared/project.yaml",
	examplePrompt,
}

// candidate returns the content of every configuration file under root, keyed by path.
func candidate(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string, len(configFiles))
	for _, rel := range configFiles {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		files[rel] = string(b)
	}
	return files
}

func replace(t *testing.T, files map[string]string, rel, old, repl string) {
	t.Helper()
	if !strings.Contains(files[rel], old) {
		t.Fatalf("%s has no %q", rel, old)
	}
	files[rel] = strings.Replace(files[rel], old, repl, 1)
}

func TestBlobSHA(t *testing.T) {
	cases := map[string]string{
		"":        "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391",
		"hello\n": "ce013625030ba8dba906f756967f9e9ca394464a",
	}
	for in, want := range cases {
		if got := blobSHA([]byte(in)); got != want {
			t.Errorf("blobSHA(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestManifestListsReadFiles(t *testing.T) {
	root := exampleRoot(t, nil)
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range cfg.Manifest.Files {
		paths = append(paths, f.Path)
		b, err := os.ReadFile(filepath.Join(root, f.Path))
		if err != nil {
			t.Fatal(err)
		}
		if want := blobSHA(b); f.SHA != want {
			t.Errorf("%s sha = %s, want %s", f.Path, f.SHA, want)
		}
	}
	want := slices.Sorted(slices.Values(configFiles))
	if !slices.Equal(paths, want) {
		t.Fatalf("manifest paths = %v, want %v", paths, want)
	}
	if cfg.Manifest.LoadedAt.IsZero() {
		t.Fatal("loadedAt not set")
	}
}

func TestCheckExampleIsValid(t *testing.T) {
	root := exampleRoot(t, nil)
	if vs := Check(root, candidate(t, root)); len(vs) != 0 {
		t.Fatalf("violations = %v", vs)
	}
}

func TestCheckReportsViolations(t *testing.T) {
	cases := []struct {
		name string
		edit func(t *testing.T, files map[string]string)
		want Violation
	}{
		{
			"unknown key",
			func(t *testing.T, files map[string]string) {
				replace(t, files, "guardians/bases.yaml", "    branch: main\n", "    branch: main\n    brnach: dev\n")
			},
			Violation{File: "guardians/bases.yaml", Key: "bases[0].brnach", Message: `unknown field "brnach"`},
		},
		{
			"missing key",
			func(t *testing.T, files map[string]string) {
				replace(t, files, "guardians/bases.yaml", "    branch: main\n", "")
			},
			Violation{File: "guardians/bases.yaml", Key: "bases[0].branch", Message: "missing"},
		},
		{
			"missing referenced prompt file",
			func(t *testing.T, files map[string]string) {
				delete(files, examplePrompt)
			},
			Violation{File: "guardians/guardians.yaml", Key: "guardians[0].prompt", Message: "file does not exist"},
		},
		{
			"prompt names a file outside the candidate",
			func(t *testing.T, files map[string]string) {
				replace(t, files, "guardians/guardians.yaml", "prompt: omp/prompts/product-opus.md", "prompt: omp/prompts/absent.md")
			},
			Violation{File: "guardians/guardians.yaml", Key: "guardians[0].prompt", Message: "file does not exist"},
		},
		{
			"missing yaml file",
			func(t *testing.T, files map[string]string) {
				delete(files, "guardians/bases.yaml")
			},
			Violation{File: "guardians/bases.yaml", Message: "file does not exist"},
		},
		{
			"missing launch link",
			func(t *testing.T, files map[string]string) {
				replace(t, files, "guardians/guardians.yaml", "launchLink:\n  base: http://127.0.0.1:47591\n  frontend: { kind: tern }\n", "")
			},
			Violation{File: "guardians/guardians.yaml", Key: "launchLink", Message: "missing"},
		},
		{
			"launch link base not http(s)",
			func(t *testing.T, files map[string]string) {
				replace(t, files, "guardians/guardians.yaml", "base: http://127.0.0.1:47591", "base: ftp://127.0.0.1:47599")
			},
			Violation{File: "guardians/guardians.yaml", Key: "launchLink.base", Message: `"ftp://127.0.0.1:47599" is not an http(s) URL`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := exampleRoot(t, nil)
			files := candidate(t, root)
			c.edit(t, files)
			if vs := Check(root, files); !slices.Equal(vs, []Violation{c.want}) {
				t.Fatalf("violations = %+v, want [%+v]", vs, c.want)
			}
		})
	}
}

// TestCheckMatchesLoad pins that a candidate reports the same violations, in the same words, as the
// startup load of the same bytes.
func TestCheckMatchesLoad(t *testing.T) {
	cases := map[string]map[string][2]string{
		"unknown key":        {"guardians/bases.yaml": {"    branch: main\n", "    branch: main\n    brnach: dev\n"}},
		"missing key":        {"guardians/bases.yaml": {"    branch: main\n", ""}},
		"zero exit grace":    {"guardians/runtime.yaml": {"exitGraceSeconds: 30", "exitGraceSeconds: 0"}},
		"missing shared key": {"shared/repos.yaml": {"omp: []\n", ""}},
		"missing prompt":     {"guardians/guardians.yaml": {"omp/prompts/product-opus.md", "omp/prompts/absent.md"}},
	}
	for name, edits := range cases {
		t.Run(name, func(t *testing.T) {
			root := exampleRoot(t, edits)
			_, err := Load(root)
			if err == nil {
				t.Fatal("Load accepted the edited configuration")
			}
			var want []string
			for _, v := range Check(root, candidate(t, root)) {
				want = append(want, v.Error())
			}
			if got := err.Error(); got != strings.Join(want, "\n") {
				t.Fatalf("Load = %q\nCheck = %q", got, strings.Join(want, "\n"))
			}
		})
	}
}
