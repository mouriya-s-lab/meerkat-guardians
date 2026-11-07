/**
 * Acceptance driver for oh-my-omp/pm-guardians#16: the plugin's gh data pipeline.
 *
 * It loads plugin/index.ts from two checkouts with a stub `pi` whose `exec` forwards to the real
 * `gh` CLI, records what each `gh` call hands to Node (stdout bytes, elements, fields) and what the
 * tool writes (argv with the `--input` file, and the request body), then compares the two checkouts
 * on one scenario over the same fixtures.
 *
 *   bun plugin/driver.mts --base <checkout> --head <checkout> --scenario <name>
 *
 * Scenarios: writes, reply-scan, escalate-scan, replace-scan, marker.
 *
 * Fixtures live in the scratch repositories oh-my-omp/pm-e2e-code and oh-my-omp/pm-e2e-agenda and
 * are deleted again. Writes reach GitHub for real; each checkout runs after the other one's writes
 * were removed, so both start from the same state.
 *
 * GitHub assigns issue numbers, comment ids, node ids and project item ids while a run executes;
 * those are replaced by placeholders ordered by first appearance before comparison. Equality then
 * means equal structure, and every byte the code itself controls is compared literally.
 */
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { pathToFileURL } from "node:url";

const CODE_REPO = "oh-my-omp/pm-e2e-code";
const PM_REPO = "oh-my-omp/pm-e2e-agenda";
const PROJECT = { owner: "oh-my-omp", number: 1 };
const FIELDS = {
	status: { name: "Status", options: { todo: "Todo", inProgress: "In Progress", done: "Done", superseded: "Superseded" } },
	kind: { name: "Kind", options: { human: "Human", agenda: "Agenda", implementation: "Implementation" } },
	agenda: { name: "Agenda" },
	proposer: { name: "Proposer", options: { human: "Human", guardians: "Guardians", coordination: "Coordination" } },
};

// ---------------------------------------------------------------------------------------------
// gh from this process: fixtures and cleanup.

interface Spawned {
	stdout: string;
	stderr: string;
	code: number;
}

async function spawnGh(args: string[]): Promise<Spawned> {
	const proc = Bun.spawn(["gh", ...args], { stdout: "pipe", stderr: "pipe" });
	const [stdout, stderr, code] = await Promise.all([
		new Response(proc.stdout).text(),
		new Response(proc.stderr).text(),
		proc.exited,
	]);
	return { stdout, stderr, code: code ?? 0 };
}

/** gh has reported empty bodies from GitHub as JSON errors; those, 5xx and rate limits are retried. */
const TRANSIENT = /unexpected end of JSON input|HTTP 50[234]|rate limit|secondary rate/i;
/** What a delete says when the resource is already gone. */
const GONE = /HTTP 404|Not Found|Could not resolve/i;

async function ghOnce(args: string[], acceptGone = false): Promise<string> {
	for (let attempt = 0; ; attempt++) {
		const result = await spawnGh(args);
		if (result.code === 0) return result.stdout;
		if (acceptGone && GONE.test(result.stderr)) return "";
		if (attempt >= 3 || !TRANSIENT.test(result.stderr)) {
			throw new Error(`gh ${args.join(" ")} exited ${result.code}: ${result.stderr.trim() || result.stdout.trim()}`);
		}
		await Bun.sleep(1000 * (attempt + 1));
	}
}

/** gh output as JSON; the offline fixtures are the driver's own, so their shape is the call's type. */
async function ghJson<T>(args: string[]): Promise<T> {
	const out = await ghOnce(args);
	return (out.trim() === "" ? null : JSON.parse(out)) as T;
}

const ISSUE_URL = /^https:\/\/github\.com\/([^/]+)\/([^/]+)\/issues\/(\d+)$/;
const COMMENT_URL = /^https:\/\/github\.com\/([^/]+)\/([^/]+)\/issues\/(\d+)#issuecomment-(\d+)$/;

const createIssue = (repo: string, title: string, body: string) =>
	ghJson<{ number: number; html_url: string; node_id: string }>([
		"api",
		`repos/${repo}/issues`,
		"-f",
		`title=${title}`,
		"-f",
		`body=${body}`,
		"--jq",
		"{number, html_url, node_id}",
	]);

const createComment = (repo: string, number: number, body: string) =>
	ghJson<{ id: number; html_url: string }>([
		"api",
		`repos/${repo}/issues/${number}/comments`,
		"-f",
		`body=${body}`,
		"--jq",
		"{id, html_url}",
	]);

async function removeComment(url: string): Promise<void> {
	const m = COMMENT_URL.exec(url);
	if (!m) return;
	await ghOnce(["api", "-X", "DELETE", `repos/${m[1]}/${m[2]}/issues/comments/${m[4]}`], true);
}

async function removeIssue(url: string): Promise<void> {
	const m = ISSUE_URL.exec(url);
	if (!m) return;
	await ghOnce(["issue", "delete", m[3], "-R", `${m[1]}/${m[2]}`, "--yes"], true);
}

async function reopenIssue(repo: string, number: number): Promise<void> {
	await ghOnce(["api", "-X", "PATCH", `repos/${repo}/issues/${number}`, "-f", "state=open"], true);
}

/** Removes fixtures an earlier, interrupted run of this driver left behind. */
async function sweep(): Promise<void> {
	for (const repo of [CODE_REPO, PM_REPO]) {
		const out = await ghOnce([
			"api",
			"--paginate",
			`repos/${repo}/issues?state=all&per_page=100`,
			"--jq",
			'.[] | select(.title | startswith("C16 driver")) | select(.pull_request == null) | .number',
		]);
		for (const line of out.split("\n")) {
			if (line !== "") await ghOnce(["issue", "delete", line, "-R", repo, "--yes"], true);
		}
	}
}

// ---------------------------------------------------------------------------------------------
// Recorded gh calls

interface Call {
	/** A request that changes GitHub; reads are everything else. */
	write: boolean;
	/** argv with the `--input` file path replaced by `<body>`. */
	argv: string[];
	/** The `--input` request body. */
	body: string | null;
	stdout: string;
}

interface Run {
	text: string;
	calls: Call[];
}

interface ExecResult {
	stdout: string;
	stderr: string;
	code: number;
	killed: boolean;
}

interface Tool {
	name: string;
	execute(id: string, params: unknown): Promise<{ content: { text: string }[] }>;
}

interface Plugin {
	default(pi: unknown): void;
	parseMarker(body: string | null): { kind: string; data: Record<string, unknown> } | null;
	insertBeforeMarker(body: string, line: string): string;
}

class Recorder {
	calls: Call[] = [];

	async exec(command: string, argv: string[]): Promise<ExecResult> {
		const normalized = [...argv];
		const at = normalized.indexOf("--input");
		const body = at < 0 ? null : await fs.readFile(normalized[at + 1], "utf8");
		if (at >= 0) normalized[at + 1] = "<body>";
		const proc = Bun.spawn([command, ...argv], { stdout: "pipe", stderr: "pipe" });
		const [stdout, stderr, code] = await Promise.all([
			new Response(proc.stdout).text(),
			new Response(proc.stderr).text(),
			proc.exited,
		]);
		this.calls.push({ write: isWrite(argv, body), argv: normalized, body, stdout });
		return { stdout, stderr, code: code ?? 0, killed: false };
	}
}

/** A GraphQL query carrying a mutation is a write, as is any api call with a non-GET method. */
function isWrite(argv: string[], body: string | null): boolean {
	if (argv[0] !== "api") return false;
	const method = argv.indexOf("-X");
	if (method >= 0 && argv[method + 1] !== "GET") return true;
	if (argv[1] === "graphql") return body !== null && body.includes('"mutation');
	return false;
}

/**
 * Loads one checkout's plugin. The baseline declares the two marker functions private; the driver
 * calls both checkouts' own implementations, so the copy adds only the `export` keyword.
 */
async function loadPlugin(dir: string): Promise<Plugin> {
	const source = await fs.readFile(path.join(dir, "plugin/index.ts"), "utf8");
	const exported = (text: string, name: string) =>
		text.includes(`export function ${name}(`) ? text : text.replace(`function ${name}(`, `export function ${name}(`);
	const patched = exported(exported(source, "parseMarker"), "insertBeforeMarker");
	if (!patched.includes("export function parseMarker(") || !patched.includes("export function insertBeforeMarker(")) {
		throw new Error(`driver: ${dir}/plugin/index.ts declares no parseMarker/insertBeforeMarker`);
	}
	const file = path.join(await fs.mkdtemp(path.join(os.tmpdir(), "pm-driver-")), "index.ts");
	await fs.writeFile(file, patched);
	return (await import(pathToFileURL(file).href)) as Plugin;
}

// ---------------------------------------------------------------------------------------------
// Running one tool of one checkout

async function runTool(plugin: Plugin, toolName: string, params: Record<string, unknown>, ref: string, ask: number): Promise<Run> {
	process.env.PM_KIND = "query";
	process.env.PM_META = JSON.stringify({ kind: "query", ref, ask });
	process.env.PM_GOAL_TOOL = "pm_reply,pm_replace,pm_escalate";
	process.env.PM_CONTINUE = "driver: continue";
	process.env.PM_GITHUB = JSON.stringify({
		pmRepo: PM_REPO,
		project: PROJECT,
		fields: FIELDS,
		code: [{ repo: CODE_REPO, cwd: process.cwd() }],
	});
	const rec = new Recorder();
	const tools = new Map<string, Tool>();
	const schema = (): unknown => ({ describe: () => schema(), optional: () => schema() });
	plugin.default({
		zod: { object: (shape: unknown) => ({ shape }), string: () => schema(), number: () => schema() },
		on: () => undefined,
		appendEntry: () => undefined,
		registerTool: (tool: Tool) => void tools.set(tool.name, tool),
		exec: (command: string, argv: string[]) => rec.exec(command, argv),
	});
	const tool = tools.get(toolName);
	if (!tool) throw new Error(`driver: ${toolName} was not registered`);
	const result = await tool.execute("driver", params);
	return { text: result.content.map(c => c.text).join(""), calls: rec.calls };
}

// ---------------------------------------------------------------------------------------------
// GitHub-assigned identifiers: replaced by placeholders ordered by first appearance.

interface Substitution {
	from: string;
	to: string;
}

function* objects(value: unknown): Generator<Record<string, unknown>> {
	if (Array.isArray(value)) {
		for (const item of value) yield* objects(item);
	} else if (typeof value === "object" && value !== null) {
		yield value as Record<string, unknown>;
		for (const item of Object.values(value)) yield* objects(item);
	}
}

/**
 * Identifiers GitHub assigned in this run's write responses. Reads are left out on purpose: a
 * narrowed read no longer returns the fixtures' fields, so masking them would compare a read with a
 * narrow projection instead of comparing the two checkouts.
 */
function substitutions(calls: Call[]): Substitution[] {
	const subs: Substitution[] = [];
	const seen = new Set<string>();
	const add = (from: string, to: string): void => {
		if (from === "" || seen.has(from)) return;
		seen.add(from);
		subs.push({ from, to });
	};
	let issue = 0;
	let comment = 0;
	for (const call of calls) {
		if (!call.write || call.stdout.trim() === "") continue;
		let value: unknown;
		try {
			value = JSON.parse(call.stdout);
		} catch {
			continue;
		}
		for (const object of objects(value)) {
			const url = typeof object.html_url === "string" ? object.html_url : "";
			const commentUrl = COMMENT_URL.exec(url);
			const issueUrl = ISSUE_URL.exec(url);
			if (commentUrl) {
				comment++;
				add(url, `<comment-url#${comment}>`);
				add(`issuecomment-${commentUrl[4]}`, `issuecomment-<comment#${comment}>`);
			} else if (issueUrl) {
				issue++;
				add(url, `<issue-url#${issue}>`);
				add(`${issueUrl[1]}/${issueUrl[2]}#${issueUrl[3]}`, `<issue-ref#${issue}>`);
				add(`/issues/${issueUrl[3]}`, `/issues/<issue#${issue}>`);
				add(`#${issueUrl[3]}`, `#<issue#${issue}>`);
			}
		}
	}
	return subs.sort((a, b) => b.from.length - a.from.length);
}

/**
 * GitHub's opaque ids: project, project item, issue and comment node ids. They appear in request
 * bodies the code echoes (item ids) and in ids a narrowed read still returns, so they are masked by
 * shape, in both checkouts, and numbered by first appearance.
 */
const OPAQUE_IDS = [/\bPVTI_[A-Za-z0-9_-]+/g, /\bPVT[SI]?_[A-Za-z0-9_-]+/g, /\bI[CF]?_[A-Za-z0-9_=-]{10,}/g];

function mask(text: string, subs: Substitution[]): string {
	let out = text;
	for (const pattern of OPAQUE_IDS) {
		const seen = new Map<string, string>();
		out = out.replace(pattern, found => {
			let placeholder = seen.get(found);
			if (placeholder === undefined) {
				placeholder = `<id#${seen.size + 1}>`;
				seen.set(found, placeholder);
			}
			return placeholder;
		});
	}
	for (const sub of subs) out = out.split(sub.from).join(sub.to);
	return out;
}

/** The write requests of a run: argv plus request body, both masked. */
function writesOf(run: Run, subs: Substitution[]): string[] {
	return run.calls
		.filter(call => call.write)
		.map(call => `<${mask(call.argv.join(" "), subs)}>\n${mask(call.body ?? "", subs)}`);
}

/** What one read handed to Node: the bytes, the elements and their fields. */
function shapeOf(call: Call): { bytes: number; elements: number; fields: string[] } {
	const out = call.stdout;
	let elements: unknown[] = [];
	if (out.trim() !== "") {
		if (out.trimStart().startsWith("[[")) elements = (JSON.parse(out) as unknown[][]).flat();
		else
			elements = out
				.split("\n")
				.filter(line => line !== "")
				.map(line => JSON.parse(line) as unknown);
	}
	const fields = new Set<string>();
	for (const element of elements) {
		if (typeof element === "object" && element !== null) for (const key of Object.keys(element)) fields.add(key);
	}
	return { bytes: Buffer.byteLength(out, "utf8"), elements: elements.length, fields: [...fields].sort() };
}

/** The paginated scans of a run, one line per scan. */
function scansOf(run: Run): string[] {
	return run.calls
		.filter(call => !call.write && call.argv.includes("--paginate"))
		.map(call => {
			const shape = shapeOf(call);
			return `${call.argv[call.argv.length - 1]} ${shape.bytes}B ${shape.elements} elements [${shape.fields.join(",")}]`;
		});
}

// ---------------------------------------------------------------------------------------------
// Verdict

const failures: string[] = [];

function equal(what: string, base: string, head: string): boolean {
	if (base === head) return true;
	failures.push(`${what}\n--- base ---\n${base}\n--- head ---\n${head}`);
	return false;
}

function report(): never {
	for (const failure of failures) console.log(failure);
	console.log(`DIFFERS (${failures.length})`);
	process.exit(1);
}

// ---------------------------------------------------------------------------------------------
// scenario marker

const LINK_LINE = "[启动 dispatch](http://127.0.0.1:47591/oh-my-omp/pm-e2e-code/issues/9?spec=%7B%7D)";

const CORPUS: { name: string; body: string }[] = [
	{ name: "empty body", body: "" },
	{ name: "no marker", body: "plain body with no marker" },
	{ name: "marker only", body: String.raw`<!-- pm:reply {"v":1,"ask":7} -->` },
	{ name: "multi-line with marker", body: String.raw`first line

second line

<!-- pm:reply {"v":1,"ask":7} -->` },
	{ name: "trailing whitespace after marker", body: String.raw`body

<!-- pm:replaces {"v":1,"old":"a/b#1","ask":7} -->   


  ` },
	{ name: "blank lines after marker", body: String.raw`a
<!-- pm:reply {"v":1,"ask":7} -->

` },
	{ name: "blank line after marker", body: "a\n\n" + String.raw`<!-- pm:reply {"v":1,"ask":7} -->` + "\n\n" },
	{ name: "carriage returns", body: "a\r\n\r\n" + String.raw`<!-- pm:reply {"v":1,"ask":7} -->` + "\r\n\r\n" },
	{ name: "marker in the middle", body: String.raw`marker in the middle
<!-- pm:reply {"v":1,"ask":7} -->
after` },
	{ name: "v not 1", body: String.raw`a

<!-- pm:reply {"v":2,"ask":7} -->` },
	{ name: "payload not json", body: String.raw`a

<!-- pm:reply not json -->` },
	{ name: "trailing text after marker", body: String.raw`a

<!-- pm:reply {"v":1,"ask":7} --> trailing` },
	{ name: "whitespace only", body: "\n\n\n   \t" },
	{ name: "tab indented marker", body: "a\n\n\t" + String.raw`<!-- pm:reply {"v":1,"ask":7} -->` + "\t" },
	{ name: "array payload", body: String.raw`a

<!-- pm:reply [1,2] -->` },
	{ name: "nested json", body: String.raw`a

<!-- pm:escalation {"v":1,"issue":"o/r#3","ask":7,"nested":{"a":[1,2]}} -->` },
	{ name: "brace inside a string", body: String.raw`a

<!-- pm:reply {"v":1,"ask":7,"by":"x}y"} -->` },
	{ name: "nbsp after marker", body: String.raw`a

<!-- pm:reply {"v":1,"ask":7} -->` + "\u00a0\n\u00a0" },
	{ name: "two markers on one line", body: String.raw`<!-- pm:reply {"v":1,"ask":7} --> <!-- pm:x {"v":1} -->` },
	{ name: "marker last line, no final newline", body: "a\nb\n" + String.raw`<!-- pm:reply {"v":1,"ask":7} -->` },
];

function markerScenario(base: Plugin, head: Plugin): void {
	for (const item of CORPUS) {
		equal(`parseMarker: ${item.name}`, JSON.stringify(base.parseMarker(item.body)), JSON.stringify(head.parseMarker(item.body)));
		equal(`insertBeforeMarker: ${item.name}`, base.insertBeforeMarker(item.body, LINK_LINE), head.insertBeforeMarker(item.body, LINK_LINE));
	}
	if (failures.length > 0) report();
	console.log(`${CORPUS.length} bodies, ${CORPUS.length * 2} comparisons, 0 failures`);
	console.log("MARKER EQUAL");
}

// ---------------------------------------------------------------------------------------------
// Fixtures

interface Fixture {
	issue: number;
	ref: string;
	ask: number;
	/** Issues and comments created for this case that no run may delete. */
	seeds: { issues: string[]; comments: string[] };
}

async function newFixture(title: string, askBody: string): Promise<Fixture> {
	const issue = await createIssue(CODE_REPO, title, `C16 driver fixture: ${title}. Deleted by plugin/driver.mts.`);
	const ask = await createComment(CODE_REPO, issue.number, askBody);
	return {
		issue: issue.number,
		ref: `${CODE_REPO}#${issue.number}`,
		ask: ask.id,
		seeds: { issues: [issue.html_url], comments: [ask.html_url] },
	};
}

async function dropFixture(fixture: Fixture): Promise<void> {
	for (const url of fixture.seeds.comments) await removeComment(url);
	for (const url of fixture.seeds.issues) await removeIssue(url);
}

/** Waits until a fixture shows up in the list a scan reads: GitHub's list endpoints lag behind. */
async function waitSeeds(fixture: Fixture): Promise<void> {
	const numbers = (out: string) => out.split("\n").filter(line => line !== "").map(Number);
	for (const url of fixture.seeds.issues) {
		const m = ISSUE_URL.exec(url);
		if (!m) continue;
		const repo = `${m[1]}/${m[2]}`;
		const number = Number(m[3]);
		for (let attempt = 0; ; attempt++) {
			const out = await ghOnce(["api", "--paginate", `repos/${repo}/issues?state=all&per_page=100`, "--jq", ".[] | select(.pull_request == null) | .number"]);
			if (numbers(out).includes(number)) break;
			if (attempt >= 20) throw new Error(`driver: ${repo}#${number} never reached the issue list`);
			await Bun.sleep(1000);
		}
	}
	for (const url of fixture.seeds.comments) {
		const m = COMMENT_URL.exec(url);
		if (!m) continue;
		const repo = `${m[1]}/${m[2]}`;
		const issue = Number(m[3]);
		const id = Number(m[4]);
		for (let attempt = 0; ; attempt++) {
			const out = await ghOnce(["api", "--paginate", `repos/${repo}/issues/${issue}/comments?per_page=100`, "--jq", ".[] | .id"]);
			if (numbers(out).includes(id)) break;
			if (attempt >= 20) throw new Error(`driver: ${url} never reached the comment list`);
			await Bun.sleep(1000);
		}
	}
}

/** Deletes what one run wrote and reopens the fixture issue. */
async function cleanRun(fixture: Fixture, run: Run): Promise<void> {
	const own = `https://github.com/${CODE_REPO}/issues/${fixture.issue}`;
	const comments = new Set<string>();
	const issues = new Set<string>();
	for (const call of run.calls) {
		if (!call.write || call.stdout.trim() === "") continue;
		let value: unknown;
		try {
			value = JSON.parse(call.stdout);
		} catch {
			continue;
		}
		for (const object of objects(value)) {
			const url = typeof object.html_url === "string" ? object.html_url : "";
			if (url === own) continue;
			if (COMMENT_URL.test(url)) comments.add(url);
			else if (ISSUE_URL.test(url)) issues.add(url);
		}
	}
	for (const url of comments) await removeComment(url);
	for (const url of issues) await removeIssue(url);
	await reopenIssue(CODE_REPO, fixture.issue);
}

// ---------------------------------------------------------------------------------------------
// One case: seeded fixture, one run per checkout, cleanup in between.

interface Case {
	id: string;
	tool: string;
	params: Record<string, unknown>;
	/** Runs once before the first checkout and returns the fixture both runs share. */
	seed(): Promise<Fixture>;
	/** A form that writes nothing compares its text without masking. */
	unmaskedText?: boolean;
	/** An unmasked fragment the run's text must contain: the reuse decision itself. */
	expectText?: string;
}

async function runCase(item: Case, base: Plugin, head: Plugin): Promise<void> {
	const fixture = await item.seed();
	await waitSeeds(fixture);
	const runs: Run[] = [];
	for (const plugin of [base, head]) {
		const run = await runTool(plugin, item.tool, item.params, fixture.ref, fixture.ask);
		runs.push(run);
		await cleanRun(fixture, run);
	}
	await dropFixture(fixture);
	const [baseRun, headRun] = runs;
	const baseSubs = substitutions(baseRun.calls);
	const headSubs = substitutions(headRun.calls);
	if (item.expectText) {
		for (const run of runs) if (!run.text.includes(item.expectText)) failures.push(`${item.id}: text lacks ${item.expectText}: ${run.text}`);
	}
	const writesEqual = equal(`${item.id} write requests`, writesOf(baseRun, baseSubs).join("\n"), writesOf(headRun, headSubs).join("\n"));
	const textEqual = item.unmaskedText
		? equal(`${item.id} text`, baseRun.text, headRun.text)
		: equal(`${item.id} text`, mask(baseRun.text, baseSubs), mask(headRun.text, headSubs));
	const writes = baseRun.calls.filter(call => call.write).length;
	console.log(`${item.id}: ${writes} write(s), ${writesEqual && textEqual ? "equal" : "DIFFERS"}`);
	console.log(`  scans base [${scansOf(baseRun).join(" | ")}]`);
	console.log(`  scans head [${scansOf(headRun).join(" | ")}]`);
}

// ---------------------------------------------------------------------------------------------
// scenario writes: every tool in each form its reuse lookup distinguishes.

function markerLine(kind: string, data: Record<string, unknown>): string {
	return `\n\n<!-- pm:${kind} ${JSON.stringify({ v: 1, ...data })} -->`;
}

function dispatchLink(ref: string, cwd: string): string {
	const m = /^([^/]+)\/([^#]+)#(\d+)$/.exec(ref);
	if (!m) throw new Error(`driver: ${ref} is not owner/name#n`);
	const spec = encodeURIComponent(JSON.stringify({ cwd, frontend: { kind: "tern" } }));
	return `http://127.0.0.1:47591/${m[1]}/${m[2]}/issues/${m[3]}?spec=${spec}`;
}

const REPLACE_PARAMS = {
	title: "C16 driver replacement",
	body: "C16 driver: the old fixture is replaced.\n\nWhat to build: nothing; this is a fixture.",
};

const ESCALATE_PARAMS = {
	title: "C16 driver agenda",
	body: "C16 driver: the council must decide nothing; this is a fixture.",
};

function writesCases(): Case[] {
	return [
		{
			id: "reply/create",
			tool: "pm_reply",
			params: { body: "C16 driver answer: the replacement is the one in the fixture." },
			seed: () => newFixture("C16 driver reply create", "C16 driver ask: reply, nothing settled yet."),
		},
		{
			id: "reply/settled",
			tool: "pm_reply",
			params: { body: "C16 driver answer that must not be written." },
			unmaskedText: true,
			expectText: "is already settled (reply)",
			async seed() {
				const fixture = await newFixture("C16 driver reply settled", "C16 driver ask: already answered.");
				const seed = await createComment(
					CODE_REPO,
					fixture.issue,
					"C16 seeded answer." + markerLine("reply", { ask: fixture.ask, decision: null }),
				);
				fixture.seeds.comments.push(seed.html_url);
				return fixture;
			},
		},
		{
			id: "replace/create",
			tool: "pm_replace",
			params: REPLACE_PARAMS,
			seed: () => newFixture("C16 driver replace create", "C16 driver ask: replace the issue."),
		},
		{
			id: "replace/settled",
			tool: "pm_replace",
			params: REPLACE_PARAMS,
			expectText: "already has the replaced marker",
			async seed() {
				const fixture = await newFixture("C16 driver replace settled", "C16 driver ask: already replaced.");
				const seed = await createComment(
					CODE_REPO,
					fixture.issue,
					"C16 seeded replacement." + markerLine("replaced", { ask: fixture.ask, by: `${CODE_REPO}#1` }),
				);
				fixture.seeds.comments.push(seed.html_url);
				return fixture;
			},
		},
		{
			id: "replace/reuse",
			tool: "pm_replace",
			params: REPLACE_PARAMS,
			expectText: "reused new issue ",
			async seed() {
				const fixture = await newFixture("C16 driver replace reuse", "C16 driver ask: an interrupted run left the new issue.");
				const body = "C16 driver replaced body.";
				// The candidate has to be newer than the ask: the scan window starts at the ask's second.
				await Bun.sleep(1100);
				const candidate = await createIssue(
					CODE_REPO,
					"C16 driver replacement candidate",
					body + markerLine("replaces", { old: fixture.ref, ask: fixture.ask }),
				);
				// The link has to name the candidate's own number, so it goes in after creation; with
				// the link present the run reuses the issue instead of patching it.
				await ghOnce([
					"api",
					"-X",
					"PATCH",
					`repos/${CODE_REPO}/issues/${candidate.number}`,
					"-f",
					`body=${body}\n\n[启动 dispatch](${dispatchLink(`${CODE_REPO}#${candidate.number}`, process.cwd())})` +
						markerLine("replaces", { old: fixture.ref, ask: fixture.ask }),
				]);
				fixture.seeds.issues.push(candidate.html_url);
				return fixture;
			},
		},
		{
			id: "escalate/create",
			tool: "pm_escalate",
			params: ESCALATE_PARAMS,
			seed: () => newFixture("C16 driver escalate create", "C16 driver ask: escalate, no agenda yet."),
		},
		{
			id: "escalate/settled",
			tool: "pm_escalate",
			params: ESCALATE_PARAMS,
			unmaskedText: true,
			expectText: "is already settled (escalated)",
			async seed() {
				const fixture = await newFixture("C16 driver escalate settled", "C16 driver ask: already escalated.");
				const seed = await createComment(
					CODE_REPO,
					fixture.issue,
					"C16 seeded escalation." + markerLine("escalated", { ask: fixture.ask, agenda: `${PM_REPO}#1` }),
				);
				fixture.seeds.comments.push(seed.html_url);
				return fixture;
			},
		},
		{
			id: "escalate/reuse",
			tool: "pm_escalate",
			params: ESCALATE_PARAMS,
			expectText: "reused agenda issue ",
			async seed() {
				const fixture = await newFixture("C16 driver escalate reuse", "C16 driver ask: an interrupted run raised the agenda.");
				const agenda = await createIssue(
					PM_REPO,
					"C16 driver agenda candidate",
					"C16 driver agenda body." + markerLine("escalation", { issue: fixture.ref, ask: fixture.ask }),
				);
				fixture.seeds.issues.push(agenda.html_url);
				return fixture;
			},
		},
	];
}

async function writesScenario(base: Plugin, head: Plugin): Promise<void> {
	const only = optionalArgument("case");
	for (const item of writesCases()) {
		if (only !== undefined && !item.id.includes(only)) continue;
		await runCase(item, base, head);
	}
	if (failures.length > 0) report();
	console.log("WRITES EQUAL");
}

// ---------------------------------------------------------------------------------------------
// scenario reply-scan: pm_reply on an issue carrying a comment load without markers.

const COMMENT_LOAD = 25;

async function replyScanScenario(base: Plugin, head: Plugin): Promise<void> {
	const fixture = await newFixture("C16 driver reply scan", "C16 driver ask: reply on a long comment list.");
	for (let i = 0; i < COMMENT_LOAD; i++) {
		const pad = "C16 driver comment filler ".repeat(40);
		const seed = await createComment(CODE_REPO, fixture.issue, `C16 driver comment ${i}: ${pad}`);
		fixture.seeds.comments.push(seed.html_url);
	}
	await waitSeeds(fixture);
	const runs: Run[] = [];
	for (const plugin of [base, head]) {
		const run = await runTool(plugin, "pm_reply", { body: "C16 driver answer on a long comment list." }, fixture.ref, fixture.ask);
		runs.push(run);
		await cleanRun(fixture, run);
	}
	await dropFixture(fixture);
	const [baseRun, headRun] = runs;
	console.log(`reply-scan base: ${scansOf(baseRun).join(" | ")}`);
	console.log(`reply-scan head: ${scansOf(headRun).join(" | ")}`);
	equal("reply-scan text", mask(baseRun.text, substitutions(baseRun.calls)), mask(headRun.text, substitutions(headRun.calls)));
	equal(
		"reply-scan write requests",
		writesOf(baseRun, substitutions(baseRun.calls)).join("\n"),
		writesOf(headRun, substitutions(headRun.calls)).join("\n"),
	);
	if (failures.length > 0) report();
	console.log("EQUAL");
}

// ---------------------------------------------------------------------------------------------
// scenario escalate-scan: the agenda lookup on a miss and on a hit.

async function escalateScanScenario(base: Plugin, head: Plugin): Promise<void> {
	for (const id of ["miss", "hit"]) {
		const fixture = await newFixture(`C16 driver escalate scan ${id}`, `C16 driver ask: escalate ${id}.`);
		if (id === "hit") {
			const agenda = await createIssue(
				PM_REPO,
				"C16 driver agenda for escalate scan",
				"C16 driver agenda body." + markerLine("escalation", { issue: fixture.ref, ask: fixture.ask }),
			);
			fixture.seeds.issues.push(agenda.html_url);
		}
		await waitSeeds(fixture);
		const runs: Run[] = [];
		for (const plugin of [base, head]) {
			const run = await runTool(plugin, "pm_escalate", ESCALATE_PARAMS, fixture.ref, fixture.ask);
			runs.push(run);
			await cleanRun(fixture, run);
		}
		await dropFixture(fixture);
		const [baseRun, headRun] = runs;
		console.log(`escalate-scan ${id} base: ${scansOf(baseRun).join(" | ")}`);
		console.log(`escalate-scan ${id} head: ${scansOf(headRun).join(" | ")}`);
		if (id === "hit") {
			for (const run of runs) if (!run.text.includes("reused agenda issue ")) failures.push(`escalate-scan hit did not reuse: ${run.text}`);
		}
		equal(`escalate-scan ${id} text`, mask(baseRun.text, substitutions(baseRun.calls)), mask(headRun.text, substitutions(headRun.calls)));
		equal(
			`escalate-scan ${id} write requests`,
			writesOf(baseRun, substitutions(baseRun.calls)).join("\n"),
			writesOf(headRun, substitutions(headRun.calls)).join("\n"),
		);
	}
	if (failures.length > 0) report();
	console.log("REUSED EQUAL");
}

// ---------------------------------------------------------------------------------------------
// scenario replace-scan: the since window on a miss.

const WINDOW_LOAD = 20;

async function replaceScanScenario(base: Plugin, head: Plugin): Promise<void> {
	const fixture = await newFixture("C16 driver replace scan", "C16 driver ask: replace on a long since window.");
	// The since window starts at the ask's creation second: the load has to be created after it.
	await Bun.sleep(1100);
	for (let i = 0; i < WINDOW_LOAD; i++) {
		const pad = "C16 driver issue filler ".repeat(60);
		const seed = await createIssue(CODE_REPO, `C16 driver window ${i}`, `C16 driver window issue ${i}: ${pad}`);
		fixture.seeds.issues.push(seed.html_url);
	}
	await waitSeeds(fixture);
	const runs: Run[] = [];
	for (const plugin of [base, head]) {
		const run = await runTool(plugin, "pm_replace", REPLACE_PARAMS, fixture.ref, fixture.ask);
		runs.push(run);
		await cleanRun(fixture, run);
	}
	await dropFixture(fixture);
	const [baseRun, headRun] = runs;
	console.log(`replace-scan base: ${scansOf(baseRun).join(" | ")}`);
	console.log(`replace-scan head: ${scansOf(headRun).join(" | ")}`);
	equal("replace-scan text", mask(baseRun.text, substitutions(baseRun.calls)), mask(headRun.text, substitutions(headRun.calls)));
	equal(
		"replace-scan write requests",
		writesOf(baseRun, substitutions(baseRun.calls)).join("\n"),
		writesOf(headRun, substitutions(headRun.calls)).join("\n"),
	);
	if (failures.length > 0) report();
	console.log("EQUAL");
}

// ---------------------------------------------------------------------------------------------
// main

function argument(name: string): string {
	const at = process.argv.indexOf(`--${name}`);
	if (at < 0 || at + 1 >= process.argv.length) throw new Error(`driver: --${name} is required`);
	return process.argv[at + 1];
}

function optionalArgument(name: string): string | undefined {
	const at = process.argv.indexOf(`--${name}`);
	return at < 0 || at + 1 >= process.argv.length ? undefined : process.argv[at + 1];
}

const scenario = argument("scenario");
const base = await loadPlugin(argument("base"));
const head = await loadPlugin(argument("head"));
if (scenario !== "marker") await sweep();

switch (scenario) {
	case "marker":
		markerScenario(base, head);
		break;
	case "writes":
		await writesScenario(base, head);
		break;
	case "reply-scan":
		await replyScanScenario(base, head);
		break;
	case "escalate-scan":
		await escalateScanScenario(base, head);
		break;
	case "replace-scan":
		await replaceScanScenario(base, head);
		break;
	default:
		throw new Error(`driver: unknown scenario ${scenario}`);
}
