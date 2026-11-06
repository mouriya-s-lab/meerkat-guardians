// Package config loads and validates the guardians configuration under <config-root>:
// guardians/{runtime,guardians,bases,routing,goals}.yaml and shared/repos.yaml.
// Unknown keys and missing required keys are errors that name the file and the YAML key path.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"
)

// PM tool names the plugin registers; every guardian omp child lists both after the guardian's tools.
const (
	BaseReadyTool   = "pm_base_ready"
	ReturnFactsTool = "pm_return_facts"
)

// Config is the validated configuration. All paths are absolute.
type Config struct {
	Runtime   Runtime
	Guardians []Guardian
	Bases     []Base
	Routes    []Route
	Goals     Goals
	Repos     Repos
}

type Runtime struct {
	Listen               string `yaml:"listen"`
	SessionsDir          string `yaml:"sessionsDir"`
	CheckoutsDir         string `yaml:"checkoutsDir"`
	OMP                  OMP    `yaml:"omp"`
	MaxContinuePrompts   int    `yaml:"maxContinuePrompts"`
	MaxConcurrentWarmups int    `yaml:"maxConcurrentWarmups"`
}

type OMP struct {
	Bin       string `yaml:"bin"`
	AgentDir  string `yaml:"agentDir"`
	Extension string `yaml:"extension"`
}

type Guardian struct {
	ID          string   `yaml:"id"`
	Description string   `yaml:"description"`
	Prompt      string   `yaml:"prompt"`
	Tools       []string `yaml:"tools"`
}

// ToolsArg is the --tools value shared by a guardian's bases and every fork of them.
func (g Guardian) ToolsArg() string {
	return strings.Join(append(append([]string(nil), g.Tools...), BaseReadyTool, ReturnFactsTool), ",")
}

// Base is one (guardian, repo, model) base session, expanded from a bases.yaml entry.
type Base struct {
	ID       string
	Guardian string
	Repo     string
	Branch   string
	Model    string
	Focus    string
}

// BaseID derives the base id: <guardian>.<owner>__<name>.<model with "/" -> "__" and ":" -> "_">.
func BaseID(guardian, repo, model string) string {
	return guardian + "." + strings.ReplaceAll(repo, "/", "__") + "." +
		strings.NewReplacer("/", "__", ":", "_").Replace(model)
}

type Route struct {
	Repo     string `yaml:"repo"`
	Guardian string `yaml:"guardian"`
	Tiers    Tiers  `yaml:"tiers"`
}

type Tiers struct {
	Simple  string `yaml:"simple"`
	Complex string `yaml:"complex"`
}

// Goals holds one Goal per session kind.
type Goals struct {
	Base  Goal `yaml:"base"`
	Proxy Goal `yaml:"proxy"`
}

// Goal is the completion tool of a session kind and its message templates.
type Goal struct {
	Tool     string `yaml:"tool"`
	Opening  string `yaml:"opening"`
	Continue string `yaml:"continue"`

	opening, cont *template.Template
}

// BaseVars are the template fields of the base goal.
type BaseVars struct{ Repo, Branch, Focus string }

// ProxyVars are the template fields of the proxy goal.
type ProxyVars struct{ Seat, Agenda, Repo, Question string }

// Render executes the goal's opening and continue templates.
func (g Goal) Render(vars any) (opening, cont string, err error) {
	var o, c strings.Builder
	if err := g.opening.Execute(&o, vars); err != nil {
		return "", "", err
	}
	if err := g.cont.Execute(&c, vars); err != nil {
		return "", "", err
	}
	return o.String(), c.String(), nil
}

type Repos struct {
	PM     PMRepo     `yaml:"pm"`
	Code   []CodeRepo `yaml:"code"`
	Design []string   `yaml:"design"`
	OMP    []string   `yaml:"omp"`
}

type PMRepo struct {
	Repo    string  `yaml:"repo"`
	Project Project `yaml:"project"`
}

type Project struct {
	Owner  string `yaml:"owner"`
	Number int    `yaml:"number"`
}

type CodeRepo struct {
	Repo string `yaml:"repo"`
	Cwd  string `yaml:"cwd"`
}

// Readable reports whether repo is listed in code, design or omp.
func (r Repos) Readable(repo string) bool {
	for _, c := range r.Code {
		if c.Repo == repo {
			return true
		}
	}
	for _, list := range [][]string{r.Design, r.OMP} {
		for _, name := range list {
			if name == repo {
				return true
			}
		}
	}
	return false
}

// Guardian returns the guardian with id.
func (c *Config) Guardian(id string) (Guardian, bool) {
	for _, g := range c.Guardians {
		if g.ID == id {
			return g, true
		}
	}
	return Guardian{}, false
}

// Base returns the base with id.
func (c *Config) Base(id string) (Base, bool) {
	for _, b := range c.Bases {
		if b.ID == id {
			return b, true
		}
	}
	return Base{}, false
}

// Route returns the route for repo.
func (c *Config) Route(repo string) (Route, bool) {
	for _, r := range c.Routes {
		if r.Repo == repo {
			return r, true
		}
	}
	return Route{}, false
}

type guardiansFile struct {
	Guardians []Guardian `yaml:"guardians"`
}

type basesFile struct {
	Bases []baseEntry `yaml:"bases"`
}

type baseEntry struct {
	Guardian string   `yaml:"guardian"`
	Repo     string   `yaml:"repo"`
	Branch   string   `yaml:"branch"`
	Models   []string `yaml:"models"`
	Focus    string   `yaml:"focus"`
}

type routingFile struct {
	Routes []Route `yaml:"routes"`
}

var (
	idPattern   = regexp.MustCompile(`^[a-z0-9-]+$`)
	repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// Load reads and validates the configuration under root.
func Load(root string) (*Config, error) {
	guardiansDir := filepath.Join(root, "guardians")
	var (
		rt  Runtime
		gf  guardiansFile
		bf  basesFile
		rf  routingFile
		gl  Goals
		rep Repos
	)
	for _, f := range []struct {
		rel string
		dst any
	}{
		{"guardians/runtime.yaml", &rt},
		{"guardians/guardians.yaml", &gf},
		{"guardians/bases.yaml", &bf},
		{"guardians/routing.yaml", &rf},
		{"guardians/goals.yaml", &gl},
		{"shared/repos.yaml", &rep},
	} {
		if err := decodeFile(root, f.rel, f.dst); err != nil {
			return nil, err
		}
	}

	v := &validator{}
	cfg := &Config{Runtime: rt, Guardians: gf.Guardians, Routes: rf.Routes, Goals: gl, Repos: rep}
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(guardiansDir, p)
	}

	// runtime.yaml
	{
		f := v.file("guardians/runtime.yaml")
		r := &cfg.Runtime
		f.str("listen", r.Listen)
		f.str("sessionsDir", r.SessionsDir)
		f.str("checkoutsDir", r.CheckoutsDir)
		f.str("omp.bin", r.OMP.Bin)
		f.str("omp.agentDir", r.OMP.AgentDir)
		f.str("omp.extension", r.OMP.Extension)
		f.positive("maxContinuePrompts", r.MaxContinuePrompts)
		f.positive("maxConcurrentWarmups", r.MaxConcurrentWarmups)
		r.SessionsDir = resolve(r.SessionsDir)
		r.CheckoutsDir = resolve(r.CheckoutsDir)
		r.OMP.AgentDir = resolve(r.OMP.AgentDir)
		r.OMP.Extension = resolve(r.OMP.Extension)
		if r.OMP.Extension != "" {
			f.exists("omp.extension", r.OMP.Extension)
		}
	}

	// shared/repos.yaml
	{
		f := v.file("shared/repos.yaml")
		f.repo("pm.repo", rep.PM.Repo)
		f.str("pm.project.owner", rep.PM.Project.Owner)
		f.positive("pm.project.number", rep.PM.Project.Number)
		f.present("code", rep.Code != nil)
		for i, c := range rep.Code {
			f.repo(fmt.Sprintf("code[%d].repo", i), c.Repo)
			f.str(fmt.Sprintf("code[%d].cwd", i), c.Cwd)
		}
		f.present("design", rep.Design != nil)
		for i, name := range rep.Design {
			f.repo(fmt.Sprintf("design[%d]", i), name)
		}
		f.present("omp", rep.OMP != nil)
		for i, name := range rep.OMP {
			f.repo(fmt.Sprintf("omp[%d]", i), name)
		}
	}

	// guardians.yaml
	guardianIDs := map[string]bool{}
	{
		f := v.file("guardians/guardians.yaml")
		f.nonEmpty("guardians", len(cfg.Guardians))
		for i := range cfg.Guardians {
			g := &cfg.Guardians[i]
			p := fmt.Sprintf("guardians[%d]", i)
			switch {
			case g.ID == "":
				f.missing(p + ".id")
			case !idPattern.MatchString(g.ID):
				f.errorf(p+".id", "%q does not match [a-z0-9-]+", g.ID)
			case guardianIDs[g.ID]:
				f.errorf(p+".id", "duplicate guardian %q", g.ID)
			}
			guardianIDs[g.ID] = true
			f.str(p+".description", g.Description)
			f.str(p+".prompt", g.Prompt)
			g.Prompt = resolve(g.Prompt)
			if g.Prompt != "" {
				f.exists(p+".prompt", g.Prompt)
			}
			f.nonEmpty(p+".tools", len(g.Tools))
			for j, t := range g.Tools {
				f.str(fmt.Sprintf("%s.tools[%d]", p, j), t)
			}
		}
	}

	// bases.yaml
	baseKeys := map[string]bool{} // guardian|repo|model
	{
		f := v.file("guardians/bases.yaml")
		f.nonEmpty("bases", len(bf.Bases))
		for i, b := range bf.Bases {
			p := fmt.Sprintf("bases[%d]", i)
			if f.str(p+".guardian", b.Guardian) && !guardianIDs[b.Guardian] {
				f.errorf(p+".guardian", "unknown guardian %q", b.Guardian)
			}
			if f.repo(p+".repo", b.Repo) && !rep.Readable(b.Repo) {
				f.errorf(p+".repo", "%s is not listed in shared/repos.yaml code, design or omp", b.Repo)
			}
			f.str(p+".branch", b.Branch)
			f.nonEmpty(p+".models", len(b.Models))
			for j, m := range b.Models {
				mp := fmt.Sprintf("%s.models[%d]", p, j)
				if !f.str(mp, m) {
					continue
				}
				key := b.Guardian + "|" + b.Repo + "|" + m
				if baseKeys[key] {
					f.errorf(mp, "duplicate base for guardian %s, repo %s, model %s", b.Guardian, b.Repo, m)
					continue
				}
				baseKeys[key] = true
				cfg.Bases = append(cfg.Bases, Base{
					ID: BaseID(b.Guardian, b.Repo, m), Guardian: b.Guardian, Repo: b.Repo,
					Branch: b.Branch, Model: m, Focus: b.Focus,
				})
			}
		}
	}

	// routing.yaml
	{
		f := v.file("guardians/routing.yaml")
		f.nonEmpty("routes", len(cfg.Routes))
		routed := map[string]bool{}
		for i, r := range cfg.Routes {
			p := fmt.Sprintf("routes[%d]", i)
			if f.repo(p+".repo", r.Repo) {
				if routed[r.Repo] {
					f.errorf(p+".repo", "repo %s is routed more than once", r.Repo)
				}
				routed[r.Repo] = true
			}
			if f.str(p+".guardian", r.Guardian) && !guardianIDs[r.Guardian] {
				f.errorf(p+".guardian", "unknown guardian %q", r.Guardian)
			}
			for _, t := range []struct{ key, model string }{{"simple", r.Tiers.Simple}, {"complex", r.Tiers.Complex}} {
				tp := p + ".tiers." + t.key
				if f.str(tp, t.model) && !baseKeys[r.Guardian+"|"+r.Repo+"|"+t.model] {
					f.errorf(tp, "no base in guardians/bases.yaml for guardian %s, repo %s, model %s", r.Guardian, r.Repo, t.model)
				}
			}
		}
	}

	// goals.yaml
	{
		f := v.file("guardians/goals.yaml")
		f.goal("base", &cfg.Goals.Base, BaseVars{})
		f.goal("proxy", &cfg.Goals.Proxy, ProxyVars{})
	}

	if err := errors.Join(v.errs...); err != nil {
		return nil, err
	}
	return cfg, nil
}

type validator struct{ errs []error }

type fileValidator struct {
	v    *validator
	name string
}

func (v *validator) file(name string) fileValidator { return fileValidator{v, name} }

func (f fileValidator) errorf(path, format string, args ...any) {
	f.v.errs = append(f.v.errs, fmt.Errorf("%s: %s: %s", f.name, path, fmt.Sprintf(format, args...)))
}

func (f fileValidator) missing(path string) { f.errorf(path, "missing") }

func (f fileValidator) str(path, s string) bool {
	if strings.TrimSpace(s) == "" {
		f.missing(path)
		return false
	}
	return true
}

func (f fileValidator) present(path string, ok bool) {
	if !ok {
		f.missing(path)
	}
}

func (f fileValidator) nonEmpty(path string, n int) {
	if n == 0 {
		f.missing(path)
	}
}

func (f fileValidator) positive(path string, n int) {
	if n <= 0 {
		f.errorf(path, "missing (must be a positive integer)")
	}
}

func (f fileValidator) repo(path, s string) bool {
	if !f.str(path, s) {
		return false
	}
	if !repoPattern.MatchString(s) {
		f.errorf(path, "%q is not owner/name", s)
		return false
	}
	return true
}

func (f fileValidator) exists(path, file string) {
	if _, err := os.Stat(file); err != nil {
		f.errorf(path, "%v", err)
	}
}

func (f fileValidator) goal(path string, g *Goal, sample any) {
	f.str(path+".tool", g.Tool)
	for _, t := range []struct {
		key string
		src string
		dst **template.Template
	}{{"opening", g.Opening, &g.opening}, {"continue", g.Continue, &g.cont}} {
		tp := path + "." + t.key
		if !f.str(tp, t.src) {
			continue
		}
		tmpl, err := template.New(tp).Option("missingkey=error").Parse(t.src)
		if err == nil {
			err = tmpl.Execute(&strings.Builder{}, sample)
		}
		if err != nil {
			f.errorf(tp, "%v", err)
			continue
		}
		*t.dst = tmpl
	}
}

// decodeFile strictly decodes root/rel into dst. Decode errors are reported with the YAML key path.
func decodeFile(root, rel string, dst any) error {
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	err = yaml.UnmarshalWithOptions(b, dst, yaml.DisallowUnknownField(), yaml.Strict())
	if err == nil {
		return nil
	}
	var ye interface {
		GetMessage() string
		GetToken() *token.Token
	}
	if errors.As(err, &ye) && ye.GetToken() != nil {
		if path := keyPath(b, ye.GetToken().Position); path != "" {
			return fmt.Errorf("%s: %s: %s", rel, path, ye.GetMessage())
		}
		return fmt.Errorf("%s: line %d: %s", rel, ye.GetToken().Position.Line, ye.GetMessage())
	}
	return fmt.Errorf("%s: %w", rel, err)
}

// keyPath returns the YAML path (without the leading "$.") of the node at pos.
func keyPath(src []byte, pos *token.Position) string {
	file, err := parser.ParseBytes(src, 0)
	if err != nil || pos == nil {
		return ""
	}
	var found string
	for _, doc := range file.Docs {
		ast.Walk(visitFunc(func(n ast.Node) bool {
			if found != "" {
				return false
			}
			if tk := n.GetToken(); tk != nil && tk.Position.Line == pos.Line && tk.Position.Column == pos.Column {
				found = n.GetPath()
				return false
			}
			return true
		}), doc)
	}
	found = strings.TrimPrefix(strings.TrimPrefix(found, "$"), ".")
	return found
}

type visitFunc func(ast.Node) bool

func (f visitFunc) Visit(n ast.Node) ast.Visitor {
	if f(n) {
		return f
	}
	return nil
}
