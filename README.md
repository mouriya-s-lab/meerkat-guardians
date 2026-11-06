# pm-guardians

Pepgent PM intent guardians: a Go daemon that keeps pre-warmed headless omp **base sessions** (one per guardian × repository × model) and answers **proxy reads** from forks of them, plus the TypeScript omp plugin loaded into every guardian omp session.

- A base session reads its repository once and ends by calling `pm_base_ready`. Bases are warmed at first start (no ready session file yet) and on `POST /v1/bases/{id}/rewarm`, never automatically on repository changes.
- A proxy read forks the newest ready base (`omp --fork`), so it shares the base's system prompt, tools, plugin and model and reuses its prompt cache; it ends by calling `pm_return_facts`, whose `facts` argument is the HTTP response.
- State lives in the omp session files under `sessionsDir`; the program keeps no database.

`omprpc/` is a public Go package that `oh-my-omp/pm-council` imports: an omp RPC client (ready frame, id-correlated commands, events) and `RunSession`, the shared headless-session loop that holds a session until one of its goal tools succeeds.

## Run locally

Requires Go, `omp` on `PATH`, `yq` (mikefarah/yq v4), `git` with a GitHub credential helper (`gh auth setup-git`), and an omp login in `~/.omp/agent/agent.db`.

```sh
mkdir -p /Users/mouriya/Ext/work/pm-guardians-local
cp -R config.example /Users/mouriya/Ext/work/pm-guardians-local/config   # then edit paths if needed
scripts/prepare-omp-dir.sh /Users/mouriya/Ext/work/pm-guardians-local/config
go run ./cmd/pm-guardians --config-root /Users/mouriya/Ext/work/pm-guardians-local/config
```

```sh
curl -s localhost:7410/v1/bases
curl -s -XPOST localhost:7410/v1/proxy-reads -d '{"seat":"<seat>","model":"<model with a ready base>","repo":"<routed owner/name>","question":"列出入口文件路径","agenda":"<owner>/<pm repo>#1"}'
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
| `GET /v1/sessions?limit=n` (default 100) | `[{"id","kind","label","model","startedAt","endedAt","state":"running\|done\|failed","error","ref","parent"}]`, newest first; `parent` is the base session id a fork came from, `null` for bases | `400 invalid_request` |
| `GET /v1/sessions/{id}/log` | the session file, `application/x-ndjson` | `404 unknown_session` |

A base is `warming` while a warm run for it is live, `ready` when a session file in its directory holds a successful `pm_base_ready` result (`session`/`warmedAt` name the newest such file, which is also the fork source), and `failed` otherwise. `stale` compares the fingerprint recorded in the ready file's `pm-meta` entry (model, SHA-256 of the system prompt, `--tools`, checked-out commit) with the current configuration and the branch head on the remote (`git ls-remote`, at most once a minute per repository); a stale base stays the fork source until someone rewarms it.

## omp sessions

Every omp child runs in the base repository's checkout with

```
omp --mode rpc --session-dir <dir> --model <model> --system-prompt <guardian prompt> \
    --tools <guardian tools>,pm_base_ready,pm_return_facts \
    --trusted-extension <omp.extension> --no-skills --no-rules --no-lsp [--fork <ready base file>]
```

and `PI_CODING_AGENT_DIR=<omp.agentDir>`. `--trusted-extension` with `--no-skills --no-rules --no-lsp` keeps ambient extensions, skills, rules and LSP out of the session, so a base and its forks see the same system prompt and tool set. Session directories: `<sessionsDir>/bases/<base id>/` and `<sessionsDir>/proxy/<base id>/`.

The plugin (`plugin/index.ts`) reads `PM_KIND`, `PM_GOAL_TOOL`, `PM_CONTINUE` and `PM_META` from the environment, appends `PM_META` as a `pm-meta` custom entry at session start, registers `pm_base_ready` and `pm_return_facts`, and answers `session_stop` with `{continue: true, additionalContext: PM_CONTINUE}` until the goal tool has run. The program also re-prompts with the continue text after each `prompt_result` that arrives before the goal tool succeeded, up to `maxContinuePrompts`; a non-retryable `prompt_result` error or omp exiting fails the session. LLM retries are omp's (`retry.*` in `config.yml`).

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
| `maxConcurrentWarmups` | warm runs at once |

`guardians/guardians.yaml`: `guardians[]` with `id` (`[a-z0-9-]+`), `description`, `prompt` (system prompt file), `tools` (built-in tools kept).

`guardians/bases.yaml`: `bases[]` with `guardian`, `repo` (`owner/name`, listed in `shared/repos.yaml` code, design or omp), `branch`, `models[]`, optional `focus` (in the base opening message). One base per (guardian, repo, model); base id `<guardian>.<owner>__<name>.<model with "/" → "__" and ":" → "_">`.

`guardians/routing.yaml`: `routes[]` with `repo` (at most once), `guardian`, `tiers.simple` and `tiers.complex` (models that must have a base for the route's guardian and repo).

`guardians/goals.yaml`: `base` and `proxy`, each with `tool` (the completion tool) and Go `text/template` strings `opening` and `continue`. Fields: base `{{.Repo}} {{.Branch}} {{.Focus}}`; proxy `{{.Seat}} {{.Agenda}} {{.Repo}} {{.Question}}`.

`guardians/omp/config.yml`: omp settings copied into `omp.agentDir` by `scripts/prepare-omp-dir.sh`; the program does not read it.

`shared/repos.yaml`: `pm.repo`, `pm.project.owner`, `pm.project.number`, `code[]` (`repo`, `cwd`), `design[]`, `omp[]`.
