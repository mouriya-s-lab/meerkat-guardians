// Package config loads and validates the guardians configuration under <config-root>:
// guardians/{runtime,guardians,bases,routing,goals}.yaml and shared/{repos,project}.yaml.
// Unknown keys and missing required keys are errors that name the file and the YAML key path.
package config

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"
)

// PMTools are the tools the plugin registers. Every guardian omp child, whatever its kind, lists
// all of them after the guardian's tools, so bases and their forks share one tool set (contract §6).
var PMTools = []string{"pm_base_ready", "pm_return_facts", "pm_reply", "pm_replace", "pm_escalate"}

// Config is the validated configuration. All paths are absolute. Root is the configuration root that
// relative file references resolve against; Manifest records what was read under it.
type Config struct {
	Root       string
	Runtime    Runtime
	Guardians  []Guardian
	LaunchLink LaunchLink
	Bases      []Base
	Routes     []Route
	Goals      Goals
	Repos      Repos
	Project    ProjectConfig
	Manifest   Manifest
}

// Manifest says what a load read: every file under the configuration root with the git blob sha of its
// content (the id GitHub's contents API reports), sorted by path, and when the load ran. Host files the
// configuration names (omp.extension, a prompt outside the root) are checked on the host and are not
// listed.
type Manifest struct {
	LoadedAt time.Time `json:"loadedAt"`
	Files    []FileSHA `json:"files"`
}

// FileSHA is one manifest entry: a path under the configuration root and its git blob sha.
type FileSHA struct {
	Path string `json:"path"`
	SHA  string `json:"sha"`
}

// Violation is one validation error: the file it concerns, the YAML key path inside it (empty when the
// whole file is at fault) and what is wrong. Its Error text is the startup error line.
type Violation struct {
	File    string `json:"file"`
	Key     string `json:"key"`
	Message string `json:"message"`
}

func (v Violation) Error() string {
	if v.Key == "" {
		return v.File + ": " + v.Message
	}
	return v.File + ": " + v.Key + ": " + v.Message
}

type Runtime struct {
	Listen                string `yaml:"listen"`
	SessionsDir           string `yaml:"sessionsDir"`
	CheckoutsDir          string `yaml:"checkoutsDir"`
	OMP                   OMP    `yaml:"omp"`
	MaxContinuePrompts    int    `yaml:"maxContinuePrompts"`
	SessionTimeoutSeconds int    `yaml:"sessionTimeoutSeconds"`
	MaxConcurrentWarmups  int    `yaml:"maxConcurrentWarmups"`
}

// SessionTimeout bounds every omp session (base, proxy, query) from start to its goal tool.
func (r Runtime) SessionTimeout() time.Duration {
	return time.Duration(r.SessionTimeoutSeconds) * time.Second
}

type OMP struct {
	Bin                 string `yaml:"bin"`
	AgentDir            string `yaml:"agentDir"`
	Extension           string `yaml:"extension"`
	ReadyTimeoutSeconds int    `yaml:"readyTimeoutSeconds"`
	ExitGraceSeconds    int    `yaml:"exitGraceSeconds"`
}

// ReadyTimeout bounds how long an omp child may take to emit its ready frame.
func (o OMP) ReadyTimeout() time.Duration { return time.Duration(o.ReadyTimeoutSeconds) * time.Second }

// ExitGrace is how long an omp child may take to exit after its stdin closes before it is killed.
func (o OMP) ExitGrace() time.Duration { return time.Duration(o.ExitGraceSeconds) * time.Second }

type Guardian struct {
	ID          string   `yaml:"id"`
	Description string   `yaml:"description"`
	Prompt      string   `yaml:"prompt"`
	Tools       []string `yaml:"tools"`
}

// ToolsArg is the --tools value shared by a guardian's bases and every fork of them.
func (g Guardian) ToolsArg() string {
	return strings.Join(append(append([]string(nil), g.Tools...), PMTools...), ",")
}

// LaunchLink is the omp-dispatch launch link written into an implementation issue the plugin creates
// with pm_replace: <base>/<owner>/<repo>/issues/<n>?spec=<{"cwd":<cwd>,"frontend":<frontend>}>. It
// has meerkat-council's launchLink shape; every omp child receives it as PM_LAUNCH_LINK, in this JSON form.
type LaunchLink struct {
	Base     string   `yaml:"base" json:"base"`
	Frontend Frontend `yaml:"frontend" json:"frontend"`
}

// Frontend is the dispatch frontend a launch link starts.
type Frontend struct {
	Kind string `yaml:"kind" json:"kind"`
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

// Tier is the tier of an ask (contract §2 `ask.tier`).
type Tier string

const (
	TierSimple  Tier = "simple"
	TierComplex Tier = "complex"
)

// ParseTier returns the tier named s.
func ParseTier(s string) (Tier, bool) {
	switch t := Tier(s); t {
	case TierSimple, TierComplex:
		return t, true
	}
	return "", false
}

// Model returns the route's model for tier.
func (t Tiers) Model(tier Tier) string {
	switch tier {
	case TierSimple:
		return t.Simple
	case TierComplex:
		return t.Complex
	}
	panic(fmt.Sprintf("unhandled tier %q", tier))
}

// Goals holds one Goal per session kind.
type Goals struct {
	Base  Goal `yaml:"base"`
	Proxy Goal `yaml:"proxy"`
	Query Goal `yaml:"query"`
}

// Goal is the completion tools of a session kind (any one succeeding completes the session)
// and its message templates.
type Goal struct {
	Tools    []string `yaml:"tools"`
	Opening  string   `yaml:"opening"`
	Continue string   `yaml:"continue"`

	opening, cont *template.Template
}

// BaseVars are the template fields of the base goal.
type BaseVars struct{ Repo, Branch, Focus string }

// ProxyVars are the template fields of the proxy goal.
type ProxyVars struct{ Seat, Agenda, Repo, Question string }

// QueryVars are the template fields of the query goal.
type QueryVars struct {
	Issue, IssueTitle, IssueBody, Question string
	Tier                                   Tier
}

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
	Owner  string `yaml:"owner" json:"owner"`
	Number int    `yaml:"number" json:"number"`
}

type CodeRepo struct {
	Repo string `yaml:"repo" json:"repo"`
	Cwd  string `yaml:"cwd" json:"cwd"`
}

// CodeRepo returns the code[] entry of repo.
func (r Repos) CodeRepo(repo string) (CodeRepo, bool) {
	for _, c := range r.Code {
		if c.Repo == repo {
			return c, true
		}
	}
	return CodeRepo{}, false
}

// ProjectConfig is shared/project.yaml: the Project field and option names (appendix B). The json
// tags equal the yaml keys, so the plugin receives the same shape in PM_GITHUB.
type ProjectConfig struct {
	Fields ProjectFields `yaml:"fields" json:"fields"`
}

type ProjectFields struct {
	Status   SelectField[StatusOptions]   `yaml:"status" json:"status"`
	Kind     SelectField[KindOptions]     `yaml:"kind" json:"kind"`
	Agenda   NamedField                   `yaml:"agenda" json:"agenda"`
	Proposer SelectField[ProposerOptions] `yaml:"proposer" json:"proposer"`
}

// SelectField is a single-select Project field: its name and the names of its options.
type SelectField[O any] struct {
	Name    string `yaml:"name" json:"name"`
	Options O      `yaml:"options" json:"options"`
}

type NamedField struct {
	Name string `yaml:"name" json:"name"`
}

type StatusOptions struct {
	Todo       string `yaml:"todo" json:"todo"`
	InProgress string `yaml:"inProgress" json:"inProgress"`
	Done       string `yaml:"done" json:"done"`
	Superseded string `yaml:"superseded" json:"superseded"`
}

type KindOptions struct {
	Human          string `yaml:"human" json:"human"`
	Agenda         string `yaml:"agenda" json:"agenda"`
	Implementation string `yaml:"implementation" json:"implementation"`
}

type ProposerOptions struct {
	Human        string `yaml:"human" json:"human"`
	Guardians    string `yaml:"guardians" json:"guardians"`
	Coordination string `yaml:"coordination" json:"coordination"`
}

// Readable reports whether repo is listed in code, design or omp.
func (r Repos) Readable(repo string) bool {
	if _, ok := r.CodeRepo(repo); ok {
		return true
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
	Guardians  []Guardian `yaml:"guardians"`
	LaunchLink LaunchLink `yaml:"launchLink"`
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

// Load reads and validates the configuration under root on disk. The violations are returned joined;
// each one's text is the startup error line.
func Load(root string) (*Config, error) {
	cfg, vs := load(os.DirFS(root), root)
	if len(vs) > 0 {
		return nil, joinViolations(vs)
	}
	return cfg, nil
}

func joinViolations(vs []Violation) error {
	errs := make([]error, len(vs))
	for i, v := range vs {
		errs[i] = v
	}
	return errors.Join(errs...)
}

// load reads and validates the configuration in fsys, whose directory on the host is root. It returns the
// configuration as far as it was built together with every violation found; a file that cannot be read or
// decoded stops the load there.
func load(fsys fs.FS, root string) (*Config, []Violation) {
	loadedAt := time.Now().UTC()
	guardiansDir := filepath.Join(root, "guardians")
	var (
		rt  Runtime
		gf  guardiansFile
		bf  basesFile
		rf  routingFile
		gl  Goals
		rep Repos
		prj ProjectConfig
	)
	v := &validator{fsys: fsys, root: root, files: map[string]*ast.File{}, shas: map[string]string{}}
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
		{"shared/project.yaml", &prj},
	} {
		file := v.decode(f.rel, f.dst)
		if file == nil {
			return nil, v.errs
		}
		v.files[f.rel] = file
	}

	cfg := &Config{Root: root, Runtime: rt, Guardians: gf.Guardians, LaunchLink: gf.LaunchLink, Routes: rf.Routes, Goals: gl, Repos: rep, Project: prj}
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
		f.positive("omp.readyTimeoutSeconds", r.OMP.ReadyTimeoutSeconds)
		f.positive("omp.exitGraceSeconds", r.OMP.ExitGraceSeconds)
		f.positive("maxContinuePrompts", r.MaxContinuePrompts)
		f.positive("sessionTimeoutSeconds", r.SessionTimeoutSeconds)
		f.positive("maxConcurrentWarmups", r.MaxConcurrentWarmups)
		r.SessionsDir = resolve(r.SessionsDir)
		r.CheckoutsDir = resolve(r.CheckoutsDir)
		r.OMP.AgentDir = resolve(r.OMP.AgentDir)
		r.OMP.Extension = resolve(r.OMP.Extension)
		if r.OMP.Extension != "" {
			f.reference("omp.extension", r.OMP.Extension)
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

	// shared/project.yaml
	{
		f := v.file("shared/project.yaml")
		pf := prj.Fields
		for _, s := range []struct{ path, value string }{
			{"fields.status.name", pf.Status.Name},
			{"fields.status.options.todo", pf.Status.Options.Todo},
			{"fields.status.options.inProgress", pf.Status.Options.InProgress},
			{"fields.status.options.done", pf.Status.Options.Done},
			{"fields.status.options.superseded", pf.Status.Options.Superseded},
			{"fields.kind.name", pf.Kind.Name},
			{"fields.kind.options.human", pf.Kind.Options.Human},
			{"fields.kind.options.agenda", pf.Kind.Options.Agenda},
			{"fields.kind.options.implementation", pf.Kind.Options.Implementation},
			{"fields.agenda.name", pf.Agenda.Name},
			{"fields.proposer.name", pf.Proposer.Name},
			{"fields.proposer.options.human", pf.Proposer.Options.Human},
			{"fields.proposer.options.guardians", pf.Proposer.Options.Guardians},
			{"fields.proposer.options.coordination", pf.Proposer.Options.Coordination},
		} {
			f.str(s.path, s.value)
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
				f.reference(p+".prompt", g.Prompt)
			}
			f.nonEmpty(p+".tools", len(g.Tools))
			for j, t := range g.Tools {
				f.str(fmt.Sprintf("%s.tools[%d]", p, j), t)
			}
		}
		if f.has("launchLink") {
			f.url("launchLink.base", cfg.LaunchLink.Base)
			f.str("launchLink.frontend.kind", cfg.LaunchLink.Frontend.Kind)
		} else {
			f.missing("launchLink")
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
		f.goal("query", &cfg.Goals.Query, QueryVars{})
	}

	if len(v.errs) > 0 {
		return nil, v.errs
	}
	cfg.Manifest = v.manifest(loadedAt)
	return cfg, nil
}

// validator collects every violation. files holds each decoded file's syntax tree, so a check can tell an
// absent key from a present one with a zero value. shas holds the git blob sha of every file read under root.
type validator struct {
	fsys  fs.FS
	root  string
	errs  []Violation
	files map[string]*ast.File
	shas  map[string]string
}

type fileValidator struct {
	v    *validator
	name string
}

func (v *validator) file(name string) fileValidator { return fileValidator{v, name} }

// has reports whether the YAML key path (without the leading "$.") is present in the file.
func (f fileValidator) has(path string) bool {
	p, err := yaml.PathString("$." + path)
	if err != nil {
		panic(fmt.Sprintf("config: bad key path %q: %v", path, err))
	}
	node, err := p.FilterFile(f.v.files[f.name])
	return err == nil && node != nil
}

func (f fileValidator) errorf(path, format string, args ...any) {
	f.v.errs = append(f.v.errs, Violation{File: f.name, Key: path, Message: fmt.Sprintf(format, args...)})
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
	switch {
	case n > 0:
	case f.has(path):
		f.errorf(path, "must be a positive integer, got %d", n)
	default:
		f.missing(path)
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

func (f fileValidator) url(path, s string) {
	if f.str(path, s) && !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		f.errorf(path, "%q is not an http(s) URL", s)
	}
}

// reference checks that the file a key names can be read. abs is the reference resolved as at runtime. A
// file inside the root is read through the source and recorded in the manifest; any other file is a host
// file and is only stat'ed.
func (f fileValidator) reference(path, abs string) {
	if rel, ok := rootRel(f.v.root, abs); ok {
		if _, err := f.v.read(rel); err != nil {
			f.errorf(path, "%s", fsMessage(err))
		}
		return
	}
	if _, err := os.Stat(abs); err != nil {
		f.errorf(path, "%s", fsMessage(err))
	}
}

// rootRel returns the slash-separated path of abs under root, or false when abs is not inside root.
func rootRel(root, abs string) (string, bool) {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	return rel, fs.ValidPath(rel)
}

func (f fileValidator) goal(path string, g *Goal, sample any) {
	f.nonEmpty(path+".tools", len(g.Tools))
	for i, t := range g.Tools {
		if f.str(fmt.Sprintf("%s.tools[%d]", path, i), t) && !slices.Contains(PMTools, t) {
			f.errorf(fmt.Sprintf("%s.tools[%d]", path, i), "%q is not a plugin tool (%s)", t, strings.Join(PMTools, ", "))
		}
	}
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

// decode strictly decodes the file at rel into dst and returns its syntax tree. A file that cannot be read or
// decoded records its violation and returns nil. Decode errors are reported with the YAML key path.
func (v *validator) decode(rel string, dst any) *ast.File {
	b, err := v.read(rel)
	if err != nil {
		v.errs = append(v.errs, Violation{File: rel, Message: fsMessage(err)})
		return nil
	}
	err = yaml.UnmarshalWithOptions(b, dst, yaml.DisallowUnknownField(), yaml.Strict())
	if err == nil {
		file, err := parser.ParseBytes(b, 0)
		if err != nil {
			v.errs = append(v.errs, Violation{File: rel, Message: err.Error()})
			return nil
		}
		return file
	}
	var ye interface {
		GetMessage() string
		GetToken() *token.Token
	}
	if errors.As(err, &ye) && ye.GetToken() != nil {
		if path := keyPath(b, ye.GetToken().Position); path != "" {
			v.errs = append(v.errs, Violation{File: rel, Key: path, Message: ye.GetMessage()})
		} else {
			v.errs = append(v.errs, Violation{File: rel, Message: fmt.Sprintf("line %d: %s", ye.GetToken().Position.Line, ye.GetMessage())})
		}
		return nil
	}
	v.errs = append(v.errs, Violation{File: rel, Message: err.Error()})
	return nil
}

// read returns the content of the file at rel under the source and records its git blob sha.
func (v *validator) read(rel string) ([]byte, error) {
	b, err := fs.ReadFile(v.fsys, rel)
	if err != nil {
		return nil, err
	}
	v.shas[rel] = blobSHA(b)
	return b, nil
}

// manifest lists the files read, sorted by path.
func (v *validator) manifest(loadedAt time.Time) Manifest {
	files := make([]FileSHA, 0, len(v.shas))
	for p, sha := range v.shas {
		files = append(files, FileSHA{Path: p, SHA: sha})
	}
	slices.SortFunc(files, func(a, b FileSHA) int { return strings.Compare(a.Path, b.Path) })
	return Manifest{LoadedAt: loadedAt, Files: files}
}

// blobSHA is the git blob id of content: the SHA-1 of "blob <len>\0" followed by the content.
func blobSHA(content []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// fsMessage is the reason in a file-system error, without the operation and the path it names. A missing
// file reads the same whether the source is the disk or a candidate set.
func fsMessage(err error) string {
	if errors.Is(err, fs.ErrNotExist) {
		return fs.ErrNotExist.Error()
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
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
