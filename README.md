# pm-guardians

Pepgent PM intent guardians: a Go daemon that keeps pre-warmed headless omp **base sessions** (one per guardian × repository × model), answers **proxy reads** and settles workers' **decision asks** from forks of them, plus the TypeScript omp plugin loaded into every guardian omp session.

- A base session reads its repository once and ends by calling `pm_base_ready`. Bases are warmed at first start (no ready session file yet) and on `POST /v1/bases/{id}/rewarm`, never automatically on repository changes.
- A proxy read forks the newest ready base (`omp --fork`), so it shares the base's system prompt, tools, plugin and model and reuses its prompt cache; it ends by calling `pm_return_facts`, whose `facts` argument is the HTTP response.
- An ask (`POST /v1/asks`) starts a **query session**: a fork of the ready base for the implementation issue's repository and the ask's tier model. It ends by calling one of `pm_reply`, `pm_replace` or `pm_escalate`, which write the settlement to GitHub (contract §2 markers); the HTTP response only says the ask was accepted.
- State lives in the omp session files under `sessionsDir` and on GitHub; the program keeps no database. When the program's run of a session fails (omp exited, a non-retryable error, the continue limit, the session timeout), it writes the reason beside the session file as `<session file without .jsonl>.pm-failure.json` (`{"reason":"…"}`), so `/v1/sessions` and `/v1/bases` report it after a restart too. At startup it rebuilds unsettled asks from GitHub: for every open issue of each `code[]` repository it reads the comments, and every `ask` marker that no `reply`, `replaced` or `escalated` marker names, and that has no running or done query session, is started as `POST /v1/asks` would start it (asks whose base is not ready are logged and wait for the next start or POST).

`omprpc/` is a public Go package that `oh-my-omp/pm-council` imports: an omp RPC client (ready frame, id-correlated commands, events) and `RunSession`, the shared headless-session loop that holds a session until one of its goal tools succeeds.

## Run locally

Requires Go, `omp` on `PATH`, `gh`, `yq` (mikefarah/yq v4), `git` with a GitHub credential helper (`gh auth setup-git`), and an omp login in `~/.omp/agent/agent.db`.

The program and the plugin read GitHub with the token in `GH_TOKEN`; the program refuses to start without it, and the plugin's `gh` calls inherit it.

```sh
export GH_TOKEN=$(gh auth token)
mkdir -p /Users/mouriya/Ext/work/pm-guardians-local
cp -R config.example /Users/mouriya/Ext/work/pm-guardians-local/config   # then edit paths if needed
scripts/prepare-omp-dir.sh /Users/mouriya/Ext/work/pm-guardians-local/config
go run ./cmd/pm-guardians --config-root /Users/mouriya/Ext/work/pm-guardians-local/config
```

```sh
curl -s localhost:7410/v1/bases
curl -s -XPOST localhost:7410/v1/proxy-reads -d '{"seat":"<seat>","model":"<model with a ready base>","repo":"<routed owner/name>","question":"列出入口文件路径","agenda":"<owner>/<pm repo>#1"}'
curl -s -XPOST localhost:7410/v1/asks -d '{"issue":"<code owner/name>#<n>","comment":<ask comment id>,"question":"<ask text>","tier":"simple"}'
curl -s 'localhost:7410/v1/sessions?limit=20'
curl -s localhost:7410/v1/sessions/<session id>/log
curl -s -XPOST localhost:7410/v1/bases/<base id>/rewarm
```

`scripts/prepare-omp-dir.sh <config-root>` reads `omp.agentDir` from `guardians/runtime.yaml` with `yq`, copies `guardians/omp/config.yml` to `<agentDir>/config.yml` and symlinks `<agentDir>/agent.db`, `agent.db-wal`, `agent.db-shm` and (when present) `models.yml`, which defines custom providers, to the same files in `~/.omp/agent/`.

Tests: `go vet ./... && go test ./...`.

## HTTP API

Every response carries `Access-Control-Allow-Origin: *`; `OPTIONS` on any path returns `204` with `Access-Control-Allow-Methods: GET, POST, OPTIONS` and `Access-Control-Allow-Headers: Content-Type`. Errors are `{"error":"<code>","message":"…"}`.

| Endpoint | Success | Errors |
|---|---|---|
| `GET /v1/health` | `{"component":"guardians","version","startedAt"}` | — |
| `GET /v1/bases` | `[{"id","guardian","repo","model","state":"warming\|ready\|failed","session","warmedAt","error","stale","staleReason"}]` | — |
| `POST /v1/bases/{id}/rewarm` | `202` | `404 unknown_base`, `409 warming` |
| `POST /v1/proxy-reads` `{seat,model,repo,question,agenda}` | `200 {"session","facts"}` | `400 invalid_request`, `404 no_guardian`, `409 base_not_ready`, `502 session_failed` |
| `POST /v1/asks` `{issue:"owner/name#n",comment:<int>,question,tier:"simple"\|"complex"}` | `202 {"ask":"<comment>"}` when a query session starts; `200` with the same body when a query session for `comment` is already running or done | `400 invalid_request`, `404 unknown_repo` (repo not in `shared/repos.yaml` `code` or not routed), `409 base_not_ready` |
| `GET /v1/sessions?limit=n` (default 100) | `[{"id","kind","label","model","startedAt","endedAt","state":"running\|done\|failed","error","ref","parent"}]`, newest first; `error` is the program's recorded failure reason, else the last LLM error in the session file; `parent` is the base session id a fork came from, `null` for bases | `400 invalid_request` |
| `GET /v1/sessions/{id}/log` | the session file, `application/x-ndjson` | `404 unknown_session` |

A base is `warming` while a warm run for it is live, `ready` when a session file in its directory holds a successful `pm_base_ready` result (`session`/`warmedAt` name the newest such file, which is also the fork source), and `failed` otherwise. `stale` compares the fingerprint recorded in the ready file's `pm-meta` entry (model, SHA-256 of the system prompt, `--tools`, checked-out commit) with the current configuration and the branch head on the remote (`git ls-remote`, at most once a minute per repository); a stale base stays the fork source until someone rewarms it.

## omp sessions

Every omp child runs in the base repository's checkout with

```
omp --mode rpc --session-dir <dir> --model <model> --system-prompt <guardian prompt> \
    --tools <guardian tools>,pm_base_ready,pm_return_facts,pm_reply,pm_replace,pm_escalate \
    --trusted-extension <omp.extension> --no-skills --no-rules --no-lsp [--fork <ready base file>]
```

and `PI_CODING_AGENT_DIR=<omp.agentDir>`. `--trusted-extension` with `--no-skills --no-rules --no-lsp` keeps ambient extensions, skills, rules and LSP out of the session, and every kind lists the same tools, so a base and its forks see the same system prompt and tool set. Session directories: `<sessionsDir>/bases/<base id>/`, `<sessionsDir>/proxy/<base id>/` and `<sessionsDir>/query/<base id>/`.

The plugin (`plugin/index.ts`) reads `PM_KIND`, `PM_GOAL_TOOL` (comma list), `PM_CONTINUE` and `PM_META` from the environment, appends `PM_META` as a `pm-meta` custom entry at session start, registers the five PM tools, and answers `session_stop` with `{continue: true, additionalContext: PM_CONTINUE}` until one of the goal tools has run. The program also re-prompts with the continue text after each `prompt_result` that arrives before a goal tool succeeded, up to `maxContinuePrompts`; a non-retryable `prompt_result` error or omp exiting fails the session. Every session must reach its goal tool within `sessionTimeoutSeconds` of omp starting, including a turn that never ends because the model keeps calling other tools: at the limit the program kills omp and the session fails with `session timeout after <N>s` (a proxy read answers `502 session_failed`; a timed-out warm run leaves the base `failed` until someone rewarms it). Nothing is retried. LLM retries are omp's (`retry.*` in `config.yml`).

A query session's `PM_META` is `{kind:"query", ref, ask, base, model, tier, fingerprint}` (`fingerprint` copied from the fork source), and it also gets `PM_GITHUB` = `{"pmRepo","project":{"owner","number"},"fields":<shared/project.yaml fields>,"code":[{"repo","cwd"}]}`. The query tools call `gh api` (request bodies through a temp file and `--input`), resolve the Project and its field and option ids by name once per process, and look up what an interrupted earlier call already wrote before writing anything, so a retry never settles an ask twice:

- `pm_reply {body}` comments on the implementation issue with `<!-- pm:reply {"v":1,"ask":A,"decision":null} -->`.
- `pm_replace {title, body}` reuses or creates the new issue in the same repository (body ending in `<!-- pm:replaces {"v":1,"old":"<ref>","ask":A} -->`; found again by `replaces.old`/`replaces.ask` among the repository's issues updated since the ask), inserts the `[启动 dispatch](http://127.0.0.1:47591/<owner>/<name>/issues/<n>?spec=…)` link before the marker, makes it a sub-issue of the old issue's parent (none when the old issue has no parent), sets its Project `Kind=Implementation`, `Status=Todo`; then comments the body's first paragraph with `<!-- pm:replaced {"v":1,"ask":A,"by":"<new ref>"} -->` on the old issue (unless present), sets its `Status=Superseded` and closes it as not planned.
- `pm_escalate {title, body}` reuses or creates the agenda issue in `pmRepo` whose body ends with `<!-- pm:escalation {"v":1,"issue":"<ref>","ask":A} -->`, adds it to the Project with `Kind=Agenda`, `Proposer=Guardians`, and comments `<!-- pm:escalated {"v":1,"ask":A,"agenda":"<pmRepo#n>"} -->` on the implementation issue.

## Configuration reference

`--config-root` points at a directory with `guardians/` and `shared/`. Relative paths in guardians YAML resolve against `<config-root>/guardians/`. Every key is required unless marked optional; unknown keys are rejected; errors name the file and key path, e.g. `guardians/bases.yaml: bases[0].branch: missing`.

`guardians/runtime.yaml`

| Key | Meaning |
|---|---|
| `listen` | HTTP listen address |
| `sessionsDir` | root of all omp session files |
| `checkoutsDir` | git checkouts, `<checkoutsDir>/<owner>/<name>` |
| `omp.bin` | omp executable |
| `omp.agentDir` | `PI_CODING_AGENT_DIR` of every omp child |
| `omp.extension` | the plugin entry, `plugin/index.ts` |
| `omp.readyTimeoutSeconds` | seconds an omp child may take to emit its ready frame |
| `omp.exitGraceSeconds` | seconds an omp child may take to exit after its stdin closes before it is killed |
| `maxContinuePrompts` | program-side continue prompts per session before it fails |
| `sessionTimeoutSeconds` | seconds any omp session (base, proxy, query) may run, from starting omp to its goal tool, before it is killed and fails with `session timeout after <N>s` |
| `maxConcurrentWarmups` | warm runs at once |

`guardians/guardians.yaml`: `guardians[]` with `id` (`[a-z0-9-]+`), `description`, `prompt` (system prompt file), `tools` (built-in tools kept).

`guardians/bases.yaml`: `bases[]` with `guardian`, `repo` (`owner/name`, listed in `shared/repos.yaml` code, design or omp), `branch`, `models[]`, optional `focus` (in the base opening message). One base per (guardian, repo, model); base id `<guardian>.<owner>__<name>.<model with "/" → "__" and ":" → "_">`.

`guardians/routing.yaml`: `routes[]` with `repo` (at most once), `guardian`, `tiers.simple` and `tiers.complex` (models that must have a base for the route's guardian and repo).

`guardians/goals.yaml`: `base`, `proxy` and `query`, each with `tools` (the completion tools, any one of which ends the session; each must be one of the five PM tools) and Go `text/template` strings `opening` and `continue`. Fields: base `{{.Repo}} {{.Branch}} {{.Focus}}`; proxy `{{.Seat}} {{.Agenda}} {{.Repo}} {{.Question}}`; query `{{.Issue}} {{.IssueTitle}} {{.IssueBody}} {{.Question}} {{.Tier}}`.

`guardians/omp/config.yml`: omp settings copied into `omp.agentDir` by `scripts/prepare-omp-dir.sh`; the program does not read it.

`shared/repos.yaml`: `pm.repo`, `pm.project.owner`, `pm.project.number`, `code[]` (`repo`, `cwd`), `design[]`, `omp[]`.

`shared/project.yaml`: `fields.status`, `fields.kind`, `fields.proposer` (each `name` and `options` — status `todo`, `inProgress`, `done`, `superseded`; kind `human`, `agenda`, `implementation`; proposer `human`, `guardians`, `coordination`) and `fields.agenda.name`: the Project's field and option names, resolved to ids by name at first use.
