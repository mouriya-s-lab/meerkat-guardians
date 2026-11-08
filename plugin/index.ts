/**
 * meerkat-guardians omp plugin.
 *
 * The guardian program starts every omp child with:
 * - PM_KIND: "base" | "proxy" | "query"
 * - PM_GOAL_TOOL: comma list of the tools any one of whose successful call completes this session
 * - PM_CONTINUE: the continue text injected while no goal tool has been called
 * - PM_META: JSON metadata recorded as the `pm-meta` custom entry of the session; for a query
 *   session it carries `ref` (the implementation issue, owner/name#n) and `ask` (the ask comment id)
 * - PM_GITHUB (query sessions): JSON {pmRepo, project:{owner,number}, fields, code:[{repo,cwd}]}
 * - PM_LAUNCH_LINK: JSON {base, frontend:{kind}}, the configured omp-dispatch launch link that
 *   pm_replace writes into the issue it creates
 *
 * Base and fork sessions load this plugin unchanged and list the same tools, so a fork keeps the
 * base's prompt cache; only the environment differs.
 *
 * The query tools write GitHub through the `gh` CLI (`gh api`), which reads GH_TOKEN from the
 * inherited environment. Every tool first looks on GitHub for what an earlier, interrupted run of
 * it already wrote and reuses it, so a retry never writes a second settlement, issue or comment.
 */
import { randomUUID } from "node:crypto";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";

function requiredEnv(name: string): string {
	const value = process.env[name];
	if (value === undefined || value === "") throw new Error(`meerkat-guardians plugin: ${name} is not set`);
	return value;
}

// ---------------------------------------------------------------------------------------------
// Environment shapes

interface SelectField<O extends string> {
	name: string;
	options: Record<O, string>;
}

interface GitHubEnv {
	pmRepo: string;
	project: { owner: string; number: number };
	fields: {
		status: SelectField<"todo" | "inProgress" | "done" | "superseded">;
		kind: SelectField<"human" | "agenda" | "implementation">;
		agenda: { name: string };
		proposer: SelectField<"human" | "guardians" | "coordination">;
	};
	code: { repo: string; cwd: string }[];
}

interface Ref {
	owner: string;
	name: string;
	repo: string; // owner/name
	number: number;
	text: string; // owner/name#n
}

interface QueryMeta {
	ref: Ref;
	ask: number;
}

function parseRef(text: string): Ref {
	const m = /^([A-Za-z0-9_.-]+)\/([A-Za-z0-9_.-]+)#([1-9][0-9]*)$/.exec(text);
	if (!m) throw new Error(`meerkat-guardians plugin: ${JSON.stringify(text)} is not owner/name#n`);
	return { owner: m[1], name: m[2], repo: `${m[1]}/${m[2]}`, number: Number(m[3]), text };
}

function parseQueryMeta(meta: unknown): QueryMeta {
	if (typeof meta !== "object" || meta === null) throw new Error("meerkat-guardians plugin: PM_META is not an object");
	const { kind, ref, ask } = meta as Record<string, unknown>;
	if (kind !== "query" || typeof ref !== "string" || typeof ask !== "number" || !Number.isInteger(ask)) {
		throw new Error("meerkat-guardians plugin: this tool needs a query session (PM_META kind=query with ref and ask)");
	}
	return { ref: parseRef(ref), ask };
}

function parseGitHubEnv(text: string): GitHubEnv {
	const env = JSON.parse(text) as GitHubEnv;
	const f = env.fields;
	const names = [f?.status?.name, f?.kind?.name, f?.agenda?.name, f?.proposer?.name];
	if (
		typeof env.pmRepo !== "string" ||
		typeof env.project?.owner !== "string" ||
		typeof env.project?.number !== "number" ||
		names.some(n => typeof n !== "string") ||
		!Array.isArray(env.code)
	) {
		throw new Error("meerkat-guardians plugin: PM_GITHUB lacks pmRepo, project, fields or code");
	}
	return env;
}

interface LaunchLink {
	base: string;
	frontend: { kind: string };
}

function parseLaunchLink(text: string): LaunchLink {
	const link = JSON.parse(text) as Partial<LaunchLink> | null;
	const base = link?.base;
	const kind = link?.frontend?.kind;
	if (typeof base !== "string" || base === "" || typeof kind !== "string" || kind === "") {
		throw new Error("meerkat-guardians plugin: PM_LAUNCH_LINK lacks base or frontend.kind");
	}
	return { base, frontend: { kind } };
}

// ---------------------------------------------------------------------------------------------
// Markers (contract §2): last line `<!-- pm:<kind> <compact JSON> -->`, JSON with "v":1.

const MARKER_LINE = /^<!-- pm:([a-z]+) (\{.*\}) -->$/;

function marker(kind: string, data: Record<string, unknown>): string {
	return `\n\n<!-- pm:${kind} ${JSON.stringify({ v: 1, ...data })} -->`;
}

/** Whitespace as String.prototype.trim sees it, so the marker line is found without copying the body. */
function isSpace(c: number): boolean {
	return (
		(c >= 0x09 && c <= 0x0d) ||
		c === 0x20 ||
		c === 0xa0 ||
		c === 0x1680 ||
		(c >= 0x2000 && c <= 0x200a) ||
		c === 0x2028 ||
		c === 0x2029 ||
		c === 0x202f ||
		c === 0x205f ||
		c === 0x3000 ||
		c === 0xfeff
	);
}

/** The end of s[0, end) without its trailing whitespace. */
function textEnd(s: string, end: number): number {
	while (end > 0 && isSpace(s.charCodeAt(end - 1))) end--;
	return end;
}

/** True when s holds only whitespace. */
function isBlankText(s: string): boolean {
	for (let i = 0; i < s.length; i++) if (!isSpace(s.charCodeAt(i))) return false;
	return true;
}

export function parseMarker(body: string | null): { kind: string; data: Record<string, unknown> } | null {
	if (!body) return null;
	const end = textEnd(body, body.length);
	let from = end === 0 ? 0 : body.lastIndexOf("\n", end - 1) + 1;
	let to = end;
	while (from < to && isSpace(body.charCodeAt(from))) from++;
	while (to > from && isSpace(body.charCodeAt(to - 1))) to--;
	const m = MARKER_LINE.exec(body.slice(from, to));
	if (!m) return null;
	let data: unknown;
	try {
		data = JSON.parse(m[2]);
	} catch {
		return null;
	}
	if (typeof data !== "object" || data === null || Array.isArray(data)) return null;
	const record = data as Record<string, unknown>;
	return record.v === 1 ? { kind: m[1], data: record } : null;
}

/** Inserts line before the last line (the marker) of body. */
export function insertBeforeMarker(body: string, line: string): string {
	const end = textEnd(body, body.length);
	const i = end === 0 ? -1 : body.lastIndexOf("\n", end - 1);
	if (i < 0) {
		let from = 0;
		while (from < end && isSpace(body.charCodeAt(from))) from++;
		const head = end <= 1 ? 0 : textEnd(body, end - 1);
		return `${body.slice(0, head)}\n\n${line}\n\n${body.slice(from, end)}`;
	}
	let head = i;
	while (head > 0 && isSpace(body.charCodeAt(head - 1))) head--;
	return `${body.slice(0, head)}\n\n${line}\n\n${body.slice(i + 1, end).trim()}`;
}

// ---------------------------------------------------------------------------------------------
// GitHub REST/GraphQL shapes read by the tools

/** An issue, as far as the tools read one. */
interface RestIssue {
	number: number;
	node_id: string;
	html_url: string;
}

/** An issue whose body the tools read: a created or reused issue. */
interface RestIssueBody extends RestIssue {
	body: string | null;
}

type Settlement = { kind: "reply" | "replaced" | "escalated"; url: string };

/** A comment the marker scan kept: its url and the body's last line, the only part a marker occupies. */
interface MarkerComment {
	url: string;
	line: string;
}

/** The settlement of ask among the scan's markers: a reply, replaced or escalated marker naming it. */
function settlement(comments: MarkerComment[], ask: number): Settlement | null {
	for (const c of comments) {
		const m = parseMarker(c.line);
		if (!m || m.data.ask !== ask) continue;
		if (m.kind === "reply" || m.kind === "replaced" || m.kind === "escalated") return { kind: m.kind, url: c.url };
	}
	return null;
}

/** The whitespace `isSpace` recognises, as a jq character class for the scan's own trimming. */
const JQ_SPACE = String.raw`[ \t\n\r\f\u000b\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000\ufeff]`;

/**
 * jq prelude of the list scans: `bodyline` is the body's last line as `parseMarker` reads it,
 * `marker` its marker, and every scan keeps only elements this prelude can match; `parseMarker`
 * re-checks each kept element in Node.
 */
const MARKER_SCAN = String.raw`def bodyline: (.body // "") | sub("${JQ_SPACE}+$"; "") | split("\n") | last | sub("^${JQ_SPACE}+"; "") | sub("${JQ_SPACE}+$"; "");
def marker: (bodyline | capture("<!-- pm:(?<kind>[a-z]+) (?<json>[{].*[}]) -->")) // null;
def mdata: (marker | .json | try fromjson catch null);`;

/** The launch link of issue ref, started in cwd: <base>/<owner>/<name>/issues/<n>?spec=<{"cwd","frontend"}>. */
function dispatchLink(launch: LaunchLink, ref: Ref, cwd: string): string {
	const spec = encodeURIComponent(JSON.stringify({ cwd, frontend: launch.frontend }));
	return `${launch.base.replace(/\/+$/, "")}/${ref.owner}/${ref.name}/issues/${ref.number}?spec=${spec}`;
}

function firstParagraph(text: string): string {
	return text.trim().split(/\r?\n\s*\r?\n/)[0].trim();
}

export default function meerkatGuardians(pi: ExtensionAPI): void {
	const goalTools = new Set(
		requiredEnv("PM_GOAL_TOOL")
			.split(",")
			.map(t => t.trim())
			.filter(t => t !== ""),
	);
	const continueText = requiredEnv("PM_CONTINUE");
	const meta: unknown = JSON.parse(requiredEnv("PM_META"));
	const z = pi.zod;
	let goalReached = false;

	const complete = (name: string, text: string) => {
		if (goalTools.has(name)) goalReached = true;
		return { content: [{ type: "text" as const, text }] };
	};

	let githubEnv: GitHubEnv | undefined;
	const github = (): GitHubEnv => {
		githubEnv ??= parseGitHubEnv(requiredEnv("PM_GITHUB"));
		return githubEnv;
	};

	let launchLinkEnv: LaunchLink | undefined;
	const launchLink = (): LaunchLink => {
		launchLinkEnv ??= parseLaunchLink(requiredEnv("PM_LAUNCH_LINK"));
		return launchLinkEnv;
	};

	// --- gh --------------------------------------------------------------------------------

	/** Runs `gh <args>`; a JSON body goes through a temp file and `--input`. Returns the raw stdout. */
	async function run(args: string[], body?: unknown): Promise<string> {
		const argv = [...args];
		let file: string | undefined;
		if (body !== undefined) {
			file = path.join(os.tmpdir(), `meerkat-guardians-${randomUUID()}.json`);
			await fs.writeFile(file, JSON.stringify(body));
			argv.push("--input", file);
		}
		try {
			const r = await pi.exec("gh", argv);
			if (r.code !== 0) throw new Error(`gh ${argv.join(" ")} exited ${r.code}: ${(r.stderr || r.stdout).trim()}`);
			return r.stdout;
		} finally {
			if (file) await fs.rm(file, { force: true });
		}
	}

	/** `gh <args>` decoded as one JSON value; whitespace-only output is null. */
	async function gh(args: string[], body?: unknown): Promise<unknown> {
		const out = await run(args, body);
		return isBlankText(out) ? null : JSON.parse(out);
	}

	/** The endpoint's elements as `jq` projects them: one compact JSON object per line. */
	async function scan<T>(jq: string, endpoint: string): Promise<T[]> {
		const out = await run(["api", "--paginate", "--jq", jq, endpoint]);
		const rows: T[] = [];
		for (let at = 0; at < out.length; ) {
			const nl = out.indexOf("\n", at);
			const stop = nl < 0 ? out.length : nl;
			if (stop > at) rows.push(JSON.parse(out.slice(at, stop)) as T);
			if (nl < 0) break;
			at = nl + 1;
		}
		return rows;
	}

	const rest = <T>(method: "GET" | "POST" | "PATCH", endpoint: string, body?: unknown) =>
		gh(["api", "-X", method, endpoint], body) as Promise<T>;

	async function graphql<T>(query: string, variables: Record<string, unknown>): Promise<T> {
		const res = (await gh(["api", "graphql"], { query, variables })) as { data?: T; errors?: { message: string }[] };
		if (res.errors?.length || !res.data) {
			throw new Error(`GraphQL: ${res.errors?.map(e => e.message).join("; ") ?? "no data"}`);
		}
		return res.data;
	}

	/** Comments on ref whose last line carries a marker for ask; nothing else leaves `gh`. */
	const markers = (ref: Ref, ask: number) =>
		scan<MarkerComment>(
			`${MARKER_SCAN}
.[]
| select(type == "object")
| (mdata) as $d
| select($d != null and $d.v == 1 and $d.ask == ${ask})
| {url: .html_url, line: bodyline}`,
			`repos/${ref.repo}/issues/${ref.number}/comments?per_page=100`,
		);

	// --- Project ids, resolved by name once per process ----------------------------------------

	interface ProjectIds {
		id: string;
		fields: Map<string, { id: string; options: Map<string, string> }>;
	}
	let projectIds: Promise<ProjectIds> | undefined;

	async function resolveProject(): Promise<ProjectIds> {
		const env = github();
		type Node = { id?: string; name?: string; options?: { id: string; name: string }[] };
		const data = await graphql<{
			repositoryOwner: { projectV2: { id: string; fields: { nodes: Node[] } } | null } | null;
		}>(
			`query($owner: String!, $number: Int!) {
				repositoryOwner(login: $owner) {
					... on ProjectV2Owner {
						projectV2(number: $number) {
							id
							fields(first: 100) {
								nodes {
									... on ProjectV2FieldCommon { id name }
									... on ProjectV2SingleSelectField { options { id name } }
								}
							}
						}
					}
				}
			}`,
			{ owner: env.project.owner, number: env.project.number },
		);
		const project = data.repositoryOwner?.projectV2;
		if (!project) throw new Error(`Project ${env.project.owner} #${env.project.number} not found`);
		const fields = new Map<string, { id: string; options: Map<string, string> }>();
		for (const n of project.fields.nodes) {
			if (n.id && n.name) fields.set(n.name, { id: n.id, options: new Map((n.options ?? []).map(o => [o.name, o.id])) });
		}
		const f = env.fields;
		for (const sel of [f.status, f.kind, f.proposer] as SelectField<string>[]) {
			const field = fields.get(sel.name);
			if (!field) throw new Error(`Project field ${sel.name} not found`);
			for (const option of Object.values(sel.options)) {
				if (!field.options.has(option)) throw new Error(`Project field ${sel.name} has no option ${option}`);
			}
		}
		if (!fields.has(f.agenda.name)) throw new Error(`Project field ${f.agenda.name} not found`);
		return { id: project.id, fields };
	}

	const project = (): Promise<ProjectIds> => {
		projectIds ??= resolveProject().catch(err => {
			projectIds = undefined;
			throw err;
		});
		return projectIds;
	};

	/** The issue's item in the Project, if it has one. */
	async function existingItem(contentId: string): Promise<string | undefined> {
		const p = await project();
		const data = await graphql<{ node: { projectItems: { nodes: { id: string; project: { id: string } }[] } } | null }>(
			`query($contentId: ID!) {
				node(id: $contentId) { ... on Issue { projectItems(first: 50) { nodes { id project { id } } } } }
			}`,
			{ contentId },
		);
		return data.node?.projectItems.nodes.find(n => n.project.id === p.id)?.id;
	}

	/**
	 * Adds content (an issue node id) to the Project, or returns its existing item. Another writer (dispatch, a Project
	 * workflow, an interrupted run of this tool) may have added it already or add it concurrently: GitHub then rejects
	 * the add with "Content already exists in this project", and the item is read back.
	 */
	async function projectItem(contentId: string): Promise<string> {
		const found = await existingItem(contentId);
		if (found) return found;
		const p = await project();
		try {
			const data = await graphql<{ addProjectV2ItemById: { item: { id: string } } }>(
				`mutation($projectId: ID!, $contentId: ID!) {
					addProjectV2ItemById(input: { projectId: $projectId, contentId: $contentId }) { item { id } }
				}`,
				{ projectId: p.id, contentId },
			);
			return data.addProjectV2ItemById.item.id;
		} catch (err) {
			if (!String(err).includes("Content already exists in this project")) throw err;
			const raced = await existingItem(contentId);
			if (!raced) throw err;
			return raced;
		}
	}

	async function setOption(itemId: string, fieldName: string, optionName: string): Promise<void> {
		const p = await project();
		const field = p.fields.get(fieldName);
		const optionId = field?.options.get(optionName);
		if (!field || !optionId) throw new Error(`Project field ${fieldName} option ${optionName} not resolved`);
		await graphql(
			`mutation($projectId: ID!, $itemId: ID!, $fieldId: ID!, $optionId: String!) {
				updateProjectV2ItemFieldValue(input: {
					projectId: $projectId, itemId: $itemId, fieldId: $fieldId, value: { singleSelectOptionId: $optionId }
				}) { projectV2Item { id } }
			}`,
			{ projectId: p.id, itemId, fieldId: field.id, optionId },
		);
	}

	const alreadyRecorded = (name: string, q: QueryMeta, s: Settlement) =>
		complete(name, `Ask ${q.ask} on ${q.ref.text} is already settled (${s.kind}): ${s.url}. Nothing new was written.`);

	// --- Session lifecycle ------------------------------------------------------------------

	pi.on("session_start", () => {
		pi.appendEntry("pm-meta", meta);
	});

	pi.on("session_stop", () => {
		if (goalReached) return undefined;
		return { continue: true, additionalContext: continueText };
	});

	// --- Base and proxy tools ----------------------------------------------------------------

	pi.registerTool({
		name: "pm_base_ready",
		label: "Base ready",
		description:
			"Call once you have finished reading the repository as instructed; summary = what the repo is and where the main parts live.",
		parameters: z.object({
			summary: z.string().describe("What the repository is and where its main parts live"),
		}),
		loadMode: "essential",
		approval: "read",
		async execute() {
			return complete("pm_base_ready", "recorded");
		},
	});

	pi.registerTool({
		name: "pm_return_facts",
		label: "Return facts",
		description:
			"Return the facts answering the question, each with repo-relative path and line numbers or design section; this ends your task.",
		parameters: z.object({
			facts: z.string().describe("Markdown facts, each citing a repo-relative path with line numbers or a design section"),
		}),
		loadMode: "essential",
		approval: "read",
		async execute() {
			return complete("pm_return_facts", "recorded");
		},
	});

	// --- Query tools: settle the worker's ask ------------------------------------------------

	pi.registerTool({
		name: "pm_reply",
		label: "Reply to ask",
		description:
			"Settle the worker's decision ask by replying on the implementation issue; body = the answer the worker continues with. This ends your task.",
		parameters: z.object({
			body: z.string().describe("The reply in markdown, citing repo paths with line numbers where relevant"),
		}),
		loadMode: "essential",
		approval: "write",
		async execute(_toolCallId, params) {
			const q = parseQueryMeta(meta);
			const existing = settlement(await markers(q.ref, q.ask), q.ask);
			if (existing) return alreadyRecorded("pm_reply", q, existing);
			const c = await rest<{ html_url: string }>("POST", `repos/${q.ref.repo}/issues/${q.ref.number}/comments`, {
				body: params.body + marker("reply", { ask: q.ask, decision: null }),
			});
			return complete("pm_reply", `Replied to ask ${q.ask} on ${q.ref.text}: ${c.html_url}`);
		},
	});

	pi.registerTool({
		name: "pm_replace",
		label: "Replace issue",
		description:
			"Settle the worker's decision ask by replacing the implementation issue with a new one in the same repository; the old issue is closed as not planned. title = new issue title; body = first paragraph the reason for replacing, then what to build and how to accept it. This ends your task.",
		parameters: z.object({
			title: z.string().describe("Title of the new implementation issue"),
			body: z
				.string()
				.describe("Markdown: first paragraph = why the old issue is replaced; then what to implement and the acceptance"),
		}),
		loadMode: "essential",
		approval: "write",
		async execute(_toolCallId, params) {
			const q = parseQueryMeta(meta);
			const env = github();
			const launch = launchLink();
			const code = env.code.find(c => c.repo === q.ref.repo);
			if (!code) throw new Error(`${q.ref.repo} is not a code repository in PM_GITHUB`);
			const existing = settlement(await markers(q.ref, q.ask), q.ask);
			if (existing && existing.kind !== "replaced") return alreadyRecorded("pm_replace", q, existing);
			const done: string[] = [];

			// a. An issue an interrupted run already created for this ask.
			const ask = await rest<{ created_at: string }>("GET", `repos/${q.ref.repo}/issues/comments/${q.ask}`);
			const candidates = await scan<{ number: number; line: string }>(
				`${MARKER_SCAN}
.[]
| select(type == "object" and .pull_request == null)
| (marker) as $m
| select($m != null and $m.kind == "replaces")
| (mdata) as $d
| select($d != null and $d.v == 1)
| select($d.old == ${JSON.stringify(q.ref.text)} and $d.ask == ${q.ask})
| {number: .number, line: bodyline}`,
				`repos/${q.ref.repo}/issues?state=all&since=${encodeURIComponent(ask.created_at)}&per_page=100`,
			);
			const reused = candidates.find(c => {
				const m = parseMarker(c.line);
				return m?.kind === "replaces" && m.data.old === q.ref.text && m.data.ask === q.ask;
			});
			// b. Create it, or read the body of the one an interrupted run left.
			let created: RestIssueBody;
			if (reused) {
				created = await rest<RestIssueBody>("GET", `repos/${q.ref.repo}/issues/${reused.number}`);
				done.push(`reused new issue ${created.html_url}`);
			} else {
				created = await rest<RestIssueBody>("POST", `repos/${q.ref.repo}/issues`, {
					title: params.title,
					body: params.body + marker("replaces", { old: q.ref.text, ask: q.ask }),
				});
				done.push(`created new issue ${created.html_url}`);
			}
			const newRef = parseRef(`${q.ref.repo}#${created.number}`);

			// c. Sub-issue of the old issue's parent; Project Kind=Implementation, Status=Todo, Proposer=Guardians. This
			// happens before the dispatch link is published: the link is what lets dispatch start a worker, and dispatch
			// moves Status on from there.
			const issues = await graphql<{
				repository: {
					oldIssue: { id: string; parent: { id: string; url: string } | null };
					newIssue: { id: string; parent: { id: string } | null };
				};
			}>(
				`query($owner: String!, $name: String!, $old: Int!, $new: Int!) {
					repository(owner: $owner, name: $name) {
						oldIssue: issue(number: $old) { id parent { id url } }
						newIssue: issue(number: $new) { id parent { id } }
					}
				}`,
				{ owner: q.ref.owner, name: q.ref.name, old: q.ref.number, new: created.number },
			);
			const { oldIssue, newIssue } = issues.repository;
			if (oldIssue.parent) {
				if (newIssue.parent?.id !== oldIssue.parent.id) {
					await graphql(
						`mutation($issueId: ID!, $subIssueId: ID!) {
							addSubIssue(input: { issueId: $issueId, subIssueId: $subIssueId }) { issue { id } }
						}`,
						{ issueId: oldIssue.parent.id, subIssueId: newIssue.id },
					);
				}
				done.push(`sub-issue of ${oldIssue.parent.url}`);
			} else {
				done.push(`${q.ref.text} has no parent issue, so the new issue has none`);
			}
			const f = env.fields;
			const link = dispatchLink(launch, newRef, code.cwd);
			const published = (created.body ?? "").includes(link);
			const newItem = await projectItem(newIssue.id);
			await setOption(newItem, f.kind.name, f.kind.options.implementation);
			if (!published) await setOption(newItem, f.status.name, f.status.options.todo);
			await setOption(newItem, f.proposer.name, f.proposer.options.guardians);
			done.push(
				published
					? `Project ${f.kind.name}=${f.kind.options.implementation}, ${f.proposer.name}=${f.proposer.options.guardians} (${f.status.name} left to dispatch: the link was already published)`
					: `Project ${f.kind.name}=${f.kind.options.implementation}, ${f.status.name}=${f.status.options.todo}, ${f.proposer.name}=${f.proposer.options.guardians}`,
			);

			// d. The dispatch link with the new number, before the marker: from here dispatch may start a worker.
			if (!published) {
				await rest("PATCH", `repos/${q.ref.repo}/issues/${created.number}`, {
					body: insertBeforeMarker(created.body ?? "", `[启动 dispatch](${link})`),
				});
				done.push("added the dispatch link");
			}

			// e. The old issue: replaced marker, Status=Superseded, closed as not planned.
			if (existing) {
				done.push(`old issue already has the replaced marker ${existing.url}`);
			} else {
				const c = await rest<{ html_url: string }>("POST", `repos/${q.ref.repo}/issues/${q.ref.number}/comments`, {
					body: firstParagraph(params.body) + marker("replaced", { ask: q.ask, by: newRef.text }),
				});
				done.push(`replaced marker on the old issue ${c.html_url}`);
			}
			const oldItem = await projectItem(oldIssue.id);
			await setOption(oldItem, f.status.name, f.status.options.superseded);
			await rest("PATCH", `repos/${q.ref.repo}/issues/${q.ref.number}`, { state: "closed", state_reason: "not_planned" });
			done.push(`${q.ref.text} ${f.status.name}=${f.status.options.superseded}, closed as not planned`);

			return complete("pm_replace", `Replaced ${q.ref.text} by ${newRef.text} for ask ${q.ask}:\n- ${done.join("\n- ")}`);
		},
	});

	pi.registerTool({
		name: "pm_escalate",
		label: "Escalate ask",
		description:
			"Settle the worker's decision ask by raising it as an agenda issue in the PM repository for the council to decide. title = agenda title; body = what the council must decide and its background. This ends your task.",
		parameters: z.object({
			title: z.string().describe("Title of the agenda issue"),
			body: z.string().describe("Markdown: the decision the council must make and its background"),
		}),
		loadMode: "essential",
		approval: "write",
		async execute(_toolCallId, params) {
			const q = parseQueryMeta(meta);
			const env = github();
			const existing = settlement(await markers(q.ref, q.ask), q.ask);
			if (existing) return alreadyRecorded("pm_escalate", q, existing);
			const done: string[] = [];

			const agendas = await scan<{ number: number; node_id: string; url: string; line: string }>(
				`${MARKER_SCAN}
.[]
| select(type == "object" and .pull_request == null)
| (marker) as $m
| select($m != null and $m.kind == "escalation")
| (mdata) as $d
| select($d != null and $d.v == 1)
| select($d.issue == ${JSON.stringify(q.ref.text)} and $d.ask == ${q.ask})
| {number: .number, node_id: .node_id, url: .html_url, line: bodyline}`,
				`repos/${env.pmRepo}/issues?state=all&per_page=100`,
			);
			const reused = agendas.find(a => {
				const m = parseMarker(a.line);
				return m?.kind === "escalation" && m.data.issue === q.ref.text && m.data.ask === q.ask;
			});
			let agenda: RestIssue;
			if (reused) {
				agenda = { number: reused.number, node_id: reused.node_id, html_url: reused.url };
				done.push(`reused agenda issue ${agenda.html_url}`);
			} else {
				agenda = await rest<RestIssue>("POST", `repos/${env.pmRepo}/issues`, {
					title: params.title,
					body: params.body + marker("escalation", { issue: q.ref.text, ask: q.ask }),
				});
				done.push(`created agenda issue ${agenda.html_url}`);
			}
			const agendaRef = `${env.pmRepo}#${agenda.number}`;
			const f = env.fields;
			const item = await projectItem(agenda.node_id);
			await setOption(item, f.kind.name, f.kind.options.agenda);
			await setOption(item, f.proposer.name, f.proposer.options.guardians);
			done.push(`Project ${f.kind.name}=${f.kind.options.agenda}, ${f.proposer.name}=${f.proposer.options.guardians}`);

			const c = await rest<{ html_url: string }>("POST", `repos/${q.ref.repo}/issues/${q.ref.number}/comments`, {
				body: `该询问需要议会决策，已提为议题 ${agendaRef}；议会定案后以答复或取代了结。` + marker("escalated", { ask: q.ask, agenda: agendaRef }),
			});
			done.push(`escalated marker on ${q.ref.text} ${c.html_url}`);
			return complete("pm_escalate", `Escalated ask ${q.ask} on ${q.ref.text} to ${agendaRef}:\n- ${done.join("\n- ")}`);
		},
	});
}
