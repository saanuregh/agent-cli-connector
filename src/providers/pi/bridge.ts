// Bridge extension loaded by the pi provider. It serves host-defined
// tools, gates tool calls through the host's permission flow, and registers
// MCP servers. It talks to the host through the RPC extension UI channel:
// an input dialog whose title is a per-session token carries one JSON
// request, and the host answers with one JSON value.
//
// Configuration comes from the AGENT_CLI_BRIDGE environment variable:
// {"token": string, "tools": [{name, description,
// parameters, readOnly}], "mcpServers": {name: config}}
//
// Messages that need no answer travel as status updates whose key starts
// with the token: "<token>:event" forwards subagent events, "<token>:user"
// reports the session entry of each user message, and "<token>:rewind"
// answers the rewind command. The host runs the rewind command by sending
// the prompt "/agent-cli-rewind <base64url JSON>"; Pi runs extension
// commands at once and records no turn for them.
//
// A prompt that needs an answer matching a JSON Schema ends with the line
// "<token>:output:<base64url JSON {schema}>". The input handler strips it
// before Pi records the prompt, and the run gets the structured_output
// tool, whose parameters are the schema, so Pi validates the answer and
// returns validation errors to the model.

export default function (pi: any) {
	// Pi loads extensions again whenever it replaces the session (new,
	// switch, fork, clone), so the configuration is kept for the process:
	// without it a reloaded bridge would drop the gate and host tools.
	const key = Symbol.for("agent-cli-connector.bridge");
	const store: any = globalThis;
	store[key] ??= JSON.parse(process.env.AGENT_CLI_BRIDGE ?? "{}");
	const config = store[key];
	// The configuration carries a token and MCP credentials; keep them out
	// of the environment that tools and MCP servers inherit.
	delete process.env.AGENT_CLI_BRIDGE;
	const token: string = config.token ?? "";
	if (!token) return;

	async function ask(ctx: any, request: unknown): Promise<any> {
		const raw = await ctx.ui.input(token, JSON.stringify(request));
		if (raw === undefined || raw === null || raw === "") {
			throw new Error("the host did not answer");
		}
		return JSON.parse(raw);
	}

	for (const tool of config.tools ?? []) {
		pi.registerTool({
			name: tool.name,
			label: tool.name,
			description: tool.description ?? "",
			parameters: tool.parameters ?? { type: "object", properties: {} },
			annotations: tool.readOnly ? { readOnlyHint: true } : undefined,
			async execute(toolCallId: string, params: unknown, _signal: unknown, _onUpdate: unknown, ctx: any) {
				const answer = await ask(ctx, { kind: "tool", name: tool.name, id: toolCallId, arguments: params });
				const result: any = { content: answer.content ?? [], details: answer.structured ?? undefined };
				if (answer.isError) result.isError = true;
				return result;
			},
		});
	}

	pi.on("tool_call", async (event: any, ctx: any) => {
		uiCtx = ctx;
		// The answer the host asked for needs no permission.
		if (event.toolName === outputTool && output) return undefined;
		const tool = pi.getAllTools?.().find((t: any) => t.name === event.toolName);
		const answer = await ask(ctx, {
			kind: "permission",
			toolName: event.toolName,
			toolCallId: event.toolCallId,
			input: event.input,
			// tool_search and codemode only reach other tools, whose calls
			// pass this gate themselves.
			readOnly:
				tool?.annotations?.readOnlyHint === true ||
				tool?.sourceInfo?.path === "builtin:tool-search" ||
				tool?.sourceInfo?.path === "builtin:codemode",
			source: tool?.sourceInfo?.path,
		});
		if (!answer.allow) {
			return { block: true, reason: answer.reason || `${event.toolName} was not allowed` };
		}
		if (answer.input && typeof event.input === "object" && event.input !== null) {
			for (const key of Object.keys(event.input)) delete event.input[key];
			Object.assign(event.input, answer.input);
		}
		return undefined;
	});

	const mcpErrors: string[] = [];
	for (const [name, server] of Object.entries(config.mcpServers ?? {})) {
		try {
			if (typeof pi.registerMcpServer !== "function") throw new Error("this Pi version cannot register MCP servers");
			pi.registerMcpServer(name, server);
		} catch (err) {
			mcpErrors.push(`MCP server ${name}: ${err instanceof Error ? err.message : String(err)}`);
		}
	}

	// Pi assigns a message's entry ID after the message_end handlers run,
	// and persists the same message object, so the entry is found by
	// identity at the next event. Extension handlers run before Pi prints
	// an event, so the report reaches the host before that event.
	const submitted: any[] = [];
	function reportUserEntries(ctx: any) {
		if (submitted.length === 0) return;
		const entries: any[] = ctx.sessionManager?.getEntries?.() ?? [];
		for (let i = 0; i < submitted.length; i++) {
			const message = submitted[i];
			let entry: any;
			for (let j = entries.length - 1; j >= 0 && !entry; j--) {
				if (entries[j]?.type === "message" && entries[j].message === message) entry = entries[j];
			}
			if (!entry) continue;
			submitted.splice(i--, 1);
			ctx.ui?.setStatus?.(`${token}:user`, JSON.stringify({ entryId: entry.id, timestamp: message.timestamp }));
		}
	}
	pi.on("message_end", async (event: any, _ctx: any) => {
		if (event?.message?.role === "user") submitted.push(event.message);
		return undefined;
	});
	pi.on("message_start", async (_event: unknown, ctx: any) => reportUserEntries(ctx));
	pi.on("turn_end", async (_event: unknown, ctx: any) => reportUserEntries(ctx));
	pi.on("agent_end", async (_event: unknown, ctx: any) => {
		reportUserEntries(ctx);
		submitted.length = 0;
	});

	// Rewind in place: move the session leaf to before the target user
	// message, then append an entry so that reopening the session keeps
	// the new branch, since Pi reopens a session at its last entry.
	pi.registerCommand?.("agent-cli-rewind", {
		description: "Internal command of the agent-cli-connector host",
		handler: async (args: string, ctx: any) => {
			let request: any;
			try {
				request = JSON.parse(Buffer.from(args.trim(), "base64url").toString("utf8"));
			} catch {
				return;
			}
			if (request?.token !== token) return;
			const report = (result: unknown) =>
				ctx.ui?.setStatus?.(`${token}:rewind`, JSON.stringify({ nonce: request.nonce, ...(result as object) }));
			try {
				const result = await ctx.navigateTree(request.targetId, { summarize: false });
				if (result?.cancelled) {
					report({ ok: false, cancelled: true });
					return;
				}
				pi.appendEntry("agent-cli-rewind", { targetId: request.targetId });
				report({ ok: true });
			} catch (err) {
				report({ ok: false, error: err instanceof Error ? err.message : String(err) });
			}
		},
	});

	const outputTool = "structured_output";
	const outputMarker = `\n${token}:output:`;
	let pendingOutput: any;
	let output: { schema: any; done: boolean; nudges: number } | undefined;
	function registerOutputTool(schema?: any) {
		pi.registerTool({
			name: outputTool,
			label: "Structured output",
			description:
				"Returns your final answer as structured data. Call it exactly once, as your last action, with the complete answer as its arguments.",
			parameters: schema ?? { type: "object", properties: {} },
			// Tools cannot be unregistered; a hidden tool is unreachable.
			exposure: schema ? "direct" : "hidden",
			async execute(_toolCallId: string, params: unknown) {
				if (output) output.done = true;
				return { content: [{ type: "text", text: "Structured output recorded." }], details: params, terminate: true };
			},
		});
	}
	// Registered hidden so that resumed sessions know the tool.
	registerOutputTool();
	pi.on("input", async (event: any) => {
		const text: string = event?.text ?? "";
		const i = text.lastIndexOf(outputMarker);
		if (i < 0) {
			// A new prompt without a schema; steering joins the current run.
			if (!event?.streamingBehavior) pendingOutput = undefined;
			return undefined;
		}
		try {
			pendingOutput = JSON.parse(Buffer.from(text.slice(i + outputMarker.length).trim(), "base64url").toString("utf8"));
		} catch {
			return undefined;
		}
		return { action: "transform", text: text.slice(0, i) };
	});
	// The settle hook is subscribed only during runs that want structured
	// output, because Pi rebuilds the session context for every settle
	// handler.
	let unsubscribeSettle: (() => void) | undefined;
	pi.on("before_agent_start", async (event: any) => {
		if (event?.messageType !== "user" || !pendingOutput) return undefined;
		output = { schema: pendingOutput.schema, done: false, nudges: 0 };
		pendingOutput = undefined;
		registerOutputTool(output.schema);
		unsubscribeSettle?.();
		unsubscribeSettle = pi.on("agent_before_settle", remind);
		event.systemPromptOptions?.promptGuidelines?.push(
			`Finish by calling the ${outputTool} tool exactly once with your final answer as its arguments.`,
		);
		return undefined;
	});
	// A run that ends without the answer gets up to two reminders.
	async function remind(event: any) {
		if (!output || output.done || event?.outcome !== "completed" || output.nudges >= 2) return undefined;
		output.nudges++;
		const reminder = {
			type: "custom_message",
			customType: "agent-cli-output",
			content: `Call the ${outputTool} tool now with your final answer as its arguments.`,
			display: false,
		};
		return { entries: [...(event.entries ?? []), reminder], continue: true };
	}
	// agent_end ends each agent loop, including the one before a reminder;
	// agent_settled ends the run.
	pi.on("agent_settled", async () => {
		if (!output) return undefined;
		unsubscribeSettle?.();
		unsubscribeSettle = undefined;
		output = undefined;
		registerOutputTool();
		return undefined;
	});

	// Forward subagent lifecycle events that subagent extensions publish on
	// the extension event bus. They travel as status updates keyed by the
	// token, which need no answer.
	let uiCtx: any;
	pi.on("session_start", async (_event: unknown, ctx: any) => {
		uiCtx = ctx;
		for (const message of mcpErrors.splice(0)) ctx.ui?.notify?.(message, "error");
	});
	pi.on("agent_start", async (_event: unknown, ctx: any) => {
		uiCtx = ctx;
	});
	for (const name of ["created", "started", "completed", "failed", "steered", "compacted"]) {
		pi.events?.on?.(`subagents:${name}`, (payload: unknown) => {
			uiCtx?.ui?.setStatus?.(`${token}:event`, JSON.stringify({ event: name, payload }));
		});
	}
}
