/**
 * pm-guardians omp plugin.
 *
 * The guardian program starts every omp child with:
 * - PM_KIND: "base" | "proxy"
 * - PM_GOAL_TOOL: the tool whose successful call completes this session
 * - PM_CONTINUE: the continue text injected while the goal tool has not been called
 * - PM_META: JSON metadata recorded as the `pm-meta` custom entry of the session
 *
 * Base and fork sessions load this plugin unchanged and list the same tools, so a fork
 * keeps the base's prompt cache; only the environment differs.
 */
import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";

function requiredEnv(name: string): string {
	const value = process.env[name];
	if (value === undefined || value === "") throw new Error(`pm-guardians plugin: ${name} is not set`);
	return value;
}

export default function pmGuardians(pi: ExtensionAPI): void {
	const goalTool = requiredEnv("PM_GOAL_TOOL");
	const continueText = requiredEnv("PM_CONTINUE");
	const meta: unknown = JSON.parse(requiredEnv("PM_META"));
	const z = pi.zod;
	let goalReached = false;

	const recorded = (name: string) => {
		if (name === goalTool) goalReached = true;
		return { content: [{ type: "text" as const, text: "recorded" }] };
	};

	pi.on("session_start", () => {
		pi.appendEntry("pm-meta", meta);
	});

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
			return recorded("pm_base_ready");
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
			return recorded("pm_return_facts");
		},
	});

	pi.on("session_stop", () => {
		if (goalReached) return undefined;
		return { continue: true, additionalContext: continueText };
	});
}
