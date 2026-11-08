# meerkat-guardians

Meerkat intent guardians: a Go daemon that keeps pre-warmed headless omp **base sessions** (one per guardian × repository × model), answers **proxy reads** and settles workers' **decision asks** from forks of them, plus the TypeScript omp plugin loaded into every guardian omp session.

- A base session reads its repository once and ends by calling `meerkat_base_ready`. Bases are warmed at first start (no ready session file yet) and on `POST /v1/bases/{id}/rewarm`, never automatically on repository changes.
- A proxy read forks the newest ready base (`omp --fork`), so it shares the base's system prompt, tools, plugin and model and reuses its prompt cache; it ends by calling `meerkat_return_facts`, whose `facts` argument is the HTTP response.
- An ask (`POST /v1/asks`) starts a **query session**: a fork of the ready base for the implementation issue's repository and the ask's tier model. It ends by calling one of `meerkat_reply`, `meerkat_replace` or `meerkat_escalate`, which write the settlement to GitHub as marker lines; the HTTP response only says the ask was accepted.
- State lives in the omp session files under `sessionsDir` and on GitHub; the program keeps no database. When the program's run of a session fails (omp exited, a non-retryable error, the continue limit, the session timeout), it writes the reason and the moment it decided the session failed beside the session file as `<session file without .jsonl>.meerkat-failure.json` (`{"reason":"…","at":"<RFC 3339>"}`), so `/v1/sessions` and `/v1/bases` report them after a restart too; `/v1/sessions` uses `at` as the session's `endedAt` (a killed omp writes nothing more), and falls back to the session file's last entry for records written without `at`. At startup it rebuilds unsettled asks from GitHub: for every open issue of each `code[]` repository it reads the comments, and every `ask` marker that no `reply`, `replaced` or `escalated` marker names, and that has no running or done query session, is started as `POST /v1/asks` would start it (asks whose base is not ready are logged and wait for the next start or POST).

`omprpc/` is a public Go package that `mouriya-s-lab/meerkat-council` imports: an omp RPC client (ready frame, id-correlated commands, events) and `RunSession`, the shared headless-session loop that holds a session until one of its goal tools succeeds.

## fake-omp

`cmd/fake-omp` is a deterministic fake omp for single-repo acceptance runs (meerkat-council tests, guardian sessions): point a configuration's `omp.bin` at it and the session's behaviour comes entirely from the environment, so timeouts, failures and a successful goal tool are repeatable without a real LLM session. Build it with `go build -o <path> ./cmd/fake-omp`.

| Env | Meaning |
|---|---|
| `FAKE_OMP_PLAN` | comma-separated steps `goal/text/retry/fatal/loop/stall/die`, one per prompt, the last repeating; `goal` calls the goal tool, `text` completes the turn with an assistant text message and no tool call, `retry`/`fatal` end it with a retryable/non-retryable error, `loop` keeps calling a non-goal tool and never completes, `stall` acknowledges the prompt and then writes and outputs nothing until killed, `die` writes the stderr tail and exits 3 |
| `FAKE_OMP_GOAL` | the goal tool's name; default: the first entry of the comma list the starting program hands its plugin, `MEERKAT_GOAL_TOOL` (meerkat-guardians) or `MEERKAT_GOAL_TOOLS` (meerkat-council), else `meerkat_return_facts` |
| `FAKE_OMP_ARGS` | path of a JSON file whose object becomes the goal tool's arguments; default `{"facts": <prompt text>}` |
| `FAKE_OMP_DELAY_MS` | delay before each step, in milliseconds |

It consumes two arguments, every other one is ignored. `--session-dir <dir>` (required) names where the session file `<UTC time>_<uuid>.jsonl` goes; `get_state` answers with that path from the start, and as in omp the file is created with the session's first assistant message (the session header, then omp's entries for each text message, tool call and result), so a session that stalls or fails before one has no file. `--fork <source file>` creates the file before the ready frame instead, with a header whose `parentSession` is the source header's `id`; the source's entries are not copied.

## Run locally

Requires Go, `omp` on `PATH`, `gh`, `yq` (mikefarah/yq v4), `git` with a GitHub credential helper (`gh auth setup-git`), and an omp login in `~/.omp/agent/agent.db`.

The program and the plugin read GitHub with the token in `GH_TOKEN`; the program refuses to start without it, and the plugin's `gh` calls inherit it.

```sh
export GH_TOKEN=$(gh auth token)
mkdir -p /Users/mouriya/Ext/work/meerkat-guardians-local
cp -R config.example /Users/mouriya/Ext/work/meerkat-guardians-local/config   # then edit paths if needed
scripts/prepare-omp-dir.sh /Users/mouriya/Ext/work/meerkat-guardians-local/config
go run ./cmd/meerkat-guardians --config-root /Users/mouriya/Ext/work/meerkat-guardians-local/config
```

```sh
curl -s localhost:7410/v1/bases
curl -s -XPOST localhost:7410/v1/proxy-reads -d '{"seat":"<seat>","model":"<model with a ready base>","repo":"<routed owner/name>","question":"列出入口文件路径","agenda":"<owner>/<agenda repo>#1"}'
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
| `GET /v1/bases` | `[{"id","guardian","repo","model","state":"warming\|ready\|failed","session","warmedAt","error","stale","staleReason","lastWarm":{"at","outcome":"done\|failed","error"}}]` | — |
| `POST /v1/bases/{id}/rewarm` | `202` | `404 unknown_base`, `409 warming` |
| `POST /v1/proxy-reads` `{seat,model,repo,question,agenda}` | `200 {"session","facts"}` | `400 invalid_request`, `404 no_guardian`, `409 base_not_ready`, `502 session_failed` |
| `POST /v1/asks` `{issue:"owner/name#n",comment:<int>,question,tier:"simple"\|"complex"}` | `202 {"ask":"<comment>"}` when a query session starts; `200` with the same body when a query session for `comment` is already running or done | `400 invalid_request`, `404 unknown_repo` (repo not in `shared/repos.yaml` `code` or not routed), `409 base_not_ready` |
| `GET /v1/sessions?limit=n` (default 100) | `[{"id","kind","label","model","startedAt","endedAt","state":"running\|done\|failed","error","ref","parent"}]`, newest first; `endedAt` is `null` while running, the failure record's `at` when the run failed and its record has one, else the session file's last entry; `error` is the program's recorded failure reason, else the last LLM error in the session file; `parent` is the base session id a fork came from, `null` for bases | `400 invalid_request` |
| `GET /v1/sessions/{id}/log` | the session file, `application/x-ndjson` | `404 unknown_session` |
| `GET /v1/config` | `{"loadedAt","files":[{"path","sha"}]}`: every file under the config root that the running program loaded at startup (the `guardians/*.yaml` and `shared/*.yaml` files and the prompt files the guardians name), sorted by `path`; `sha` is the git blob sha of the content, as `git hash-object` and the GitHub contents API report it; no content | — |
| `POST /v1/config/check` `{"files":{"<path>":"<full text>"}}` | `200 {"ok":true}`, or `200 {"ok":false,"errors":[{"file","key","message"}]}` with the errors the startup load would report for the same content (`file` and `key` locate each one) | `400 invalid_request` for a body of any other shape, including unknown fields and data after the object |

A base is `warming` while a warm run for it is live, `ready` when a session file in its directory holds a successful `meerkat_base_ready` result (`session`/`warmedAt` name the newest such file, which is also the fork source), and `failed` otherwise; `error` says why only for a `failed` base. `stale` compares the fingerprint recorded in the ready file's `meerkat-meta` entry (model, SHA-256 of the system prompt, `--tools`, SHA-256 of the omp settings `<omp.agentDir>/config.yml`, checked-out commit) with the current configuration and the branch head on the remote (`git ls-remote`, at most once a minute per repository); `staleReason` joins the differences with `; ` (`model changed <old> -> <new>`, `system prompt changed`, `tools changed <old> -> <new>`, `omp settings changed`, `branch <branch> moved <old> -> <new>`). A ready file whose fingerprint has no omp settings hash (warmed before the program recorded it) counts as `omp settings changed`. A stale base stays the fork source until someone rewarms it; the program never rewarms on its own.

`lastWarm` is the base's latest warm attempt that left a trace in its session directory: the newest ready session (`outcome` `done`, `at` equal to `warmedAt`, `error` `null`) or the newest failure record (`outcome` `failed`, `at` and `error` the record's `at` and `reason`), whichever is later; `null` when there is neither. A rewarm that fails leaves `state` `ready` and `session` on the previous ready session while `lastWarm` reports the failure, until a later warm succeeds. While a warm run is live, `lastWarm` still names the attempt before it. It is read from the session files and failure records, so it survives a restart.

`POST /v1/config/check` runs the startup validation over the candidate set alone: `path`s are relative to the config root, and a file the request does not name does not exist. It reads and writes nothing under the config root and does not touch the running configuration or the bases. The only host files it looks at are those the configuration names outside the root (`omp.extension`, and a prompt given as a path outside the root), which it stats the same way startup does. `GET /v1/config` reports the files of the configuration loaded at startup; the program does not reload it.

## omp sessions

Every omp child runs in the base repository's checkout with

```
omp --mode rpc --session-dir <dir> --model <model> --system-prompt <guardian prompt> \
    --tools <guardian tools>,meerkat_base_ready,meerkat_return_facts,meerkat_reply,meerkat_replace,meerkat_escalate \
    --trusted-extension <omp.extension> --no-skills --no-rules --no-lsp [--fork <ready base file>]
```

and `PI_CODING_AGENT_DIR=<omp.agentDir>`. `--trusted-extension` with `--no-skills --no-rules --no-lsp` keeps ambient extensions, skills, rules and LSP out of the session, and every kind lists the same tools, so a base and its forks see the same system prompt and tool set. Session directories: `<sessionsDir>/bases/<base id>/`, `<sessionsDir>/proxy/<base id>/` and `<sessionsDir>/query/<base id>/`.

The plugin (`plugin/index.ts`) reads `MEERKAT_KIND`, `MEERKAT_GOAL_TOOL` (comma list), `MEERKAT_CONTINUE`, `MEERKAT_META` and `MEERKAT_LAUNCH_LINK` from the environment, appends `MEERKAT_META` as a `meerkat-meta` custom entry at session start, registers the five Meerkat tools, and answers `session_stop` with `{continue: true, additionalContext: MEERKAT_CONTINUE}` until one of the goal tools has run. Every omp child gets `MEERKAT_LAUNCH_LINK` = `{"base","frontend":{"kind"}}`, the `launchLink` of `guardians/guardians.yaml`. The program also re-prompts with the continue text after each `prompt_result` that arrives before a goal tool succeeded, up to `maxContinuePrompts`; a non-retryable `prompt_result` error or omp exiting fails the session. Every session must reach its goal tool within `sessionTimeoutSeconds` of omp starting, including a turn that never ends because the model keeps calling other tools: at the limit the program kills omp and the session fails with `session timeout after <N>s` (a proxy read answers `502 session_failed`; a timed-out warm run leaves the base `failed` until someone rewarms it). Nothing is retried. LLM retries are omp's (`retry.*` in `config.yml`).

A query session's `MEERKAT_META` is `{kind:"query", ref, ask, base, model, tier, fingerprint}` (`fingerprint` copied from the fork source), and it also gets `MEERKAT_GITHUB` = `{"agendaRepo","project":{"owner","number"},"fields":<shared/project.yaml fields>,"code":[{"repo","cwd"}]}`. The query tools call `gh api` (request bodies through a temp file and `--input`), resolve the Project and its field and option ids by name once per process, and look up what an interrupted earlier call already wrote before writing anything, so a retry never settles an ask twice:

- `meerkat_reply {body}` comments on the implementation issue with `<!-- meerkat:reply {"v":1,"ask":A,"decision":null} -->`.
- `meerkat_replace {title, body}` reuses or creates the new issue in the same repository (body ending in `<!-- meerkat:replaces {"v":1,"old":"<ref>","ask":A} -->`; found again by `replaces.old`/`replaces.ask` among the repository's issues updated since the ask), inserts the `[启动 dispatch](<base>/<owner>/<name>/issues/<n>?spec=<{"cwd":<code cwd>,"frontend":<frontend>}>)` link built from `MEERKAT_LAUNCH_LINK` before the marker, makes it a sub-issue of the old issue's parent (none when the old issue has no parent), sets its Project `Kind=Implementation`, `Status=Todo`, `Proposer=Guardians`; then comments the body's first paragraph with `<!-- meerkat:replaced {"v":1,"ask":A,"by":"<new ref>"} -->` on the old issue (unless present), sets its `Status=Superseded` and closes it as not planned.
- `meerkat_escalate {title, body}` reuses or creates the agenda issue in `agendaRepo` whose body ends with `<!-- meerkat:escalation {"v":1,"issue":"<ref>","ask":A} -->`, adds it to the Project with `Kind=Agenda`, `Proposer=Guardians`, and comments `<!-- meerkat:escalated {"v":1,"ask":A,"agenda":"<agendaRepo#n>"} -->` on the implementation issue.

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

`guardians/guardians.yaml`: `guardians[]` with `id` (`[a-z0-9-]+`), `description`, `prompt` (system prompt file), `tools` (built-in tools kept); `launchLink` with `base` (an `http://` or `https://` URL) and `frontend.kind` (non-empty): the omp-dispatch launch link `meerkat_replace` writes into the issue it creates, handed to every omp child as `MEERKAT_LAUNCH_LINK`. It has the shape of meerkat-council's `council/council.yaml` `launchLink`; both point at the same dispatch.

`guardians/bases.yaml`: `bases[]` with `guardian`, `repo` (`owner/name`, listed in `shared/repos.yaml` code, design or omp), `branch`, `models[]`, optional `focus` (in the base opening message). One base per (guardian, repo, model); base id `<guardian>.<owner>__<name>.<model with "/" → "__" and ":" → "_">`.

`guardians/routing.yaml`: `routes[]` with `repo` (at most once), `guardian`, `tiers.simple` and `tiers.complex` (models that must have a base for the route's guardian and repo).

`guardians/goals.yaml`: `base`, `proxy` and `query`, each with `tools` (the completion tools, any one of which ends the session; each must be one of the five Meerkat tools) and Go `text/template` strings `opening` and `continue`. Fields: base `{{.Repo}} {{.Branch}} {{.Focus}}`; proxy `{{.Seat}} {{.Agenda}} {{.Repo}} {{.Question}}`; query `{{.Issue}} {{.IssueTitle}} {{.IssueBody}} {{.Question}} {{.Tier}}`.

`guardians/omp/config.yml`: omp settings copied into `omp.agentDir` by `scripts/prepare-omp-dir.sh`; the program does not read it. It reads the copy `<omp.agentDir>/config.yml` only to hash it into the base fingerprint: a missing copy fails every warm run and, while any base is ready, `GET /v1/bases` (`500 internal`). After changing the settings, rerun the script and rewarm the bases that report `omp settings changed`.

`shared/repos.yaml`: `agenda.repo`, `agenda.project.owner`, `agenda.project.number`, `code[]` (`repo`, `cwd`), `design[]`, `omp[]`.

`shared/project.yaml`: `fields.status`, `fields.kind`, `fields.proposer` (each `name` and `options` — status `todo`, `inProgress`, `done`, `superseded`; kind `human`, `agenda`, `implementation`; proposer `human`, `guardians`, `coordination`) and `fields.agenda.name`: the Project's field and option names, resolved to ids by name at first use.
