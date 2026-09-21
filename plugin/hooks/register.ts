import type { EngineInterface, Register, ToolCallInput } from 'claude-code';

const quarry = 'quarry'; // from PATH

// Starts quarry at once when the rules already allow the call, else only once the call succeeds,
// so a call the person or classifier refuses never reaches the page, Jev or Exa
const beside = async ($: EngineInterface, { tool, tool_use_id, ...input }: ToolCallInput, argv: string[]) => {
  const run = () =>
    $.process.run([quarry, ...argv], { timeoutMs: 60_000 }).then(
      (p) => (p.exitCode === 0 ? p.stdout.trim() : ''),
      () => '',
    );
  const allowed = await $.tool.check({ tool, input }).then(
    (c) => c.decision === 'allow',
    () => false,
  );
  const early = allowed ? run() : null;
  return () => early ?? run();
};

export const register: Register = (on) => {
  // The whole skill, as quarry's own help prints it, once per conversation and again after compaction or /clear
  on('prompt.context', async ($, e, next) => {
    const help = (await $.process.run([quarry, '-h']).catch(() => null))?.stderr.trim();
    return next(help ? { ...e, blocks: [...e.blocks, { name: 'quarry', text: help }] } : e);
  });

  // WebFetch answers as before; quarry also saves the page and has Jev point to the lines that answer the prompt
  on('tool.call', { tool: 'WebFetch' }, async ($, e, next) => {
    const after = await beside($, e, ['-f', e.url, e.prompt]);
    const r = await next(e);
    if (r.result === undefined || r.isError) return r;
    const [path, ...where] = (await after()).split('\n');
    if (!path) return r;
    return {
      ...r,
      context: [
        ...(r.context ?? []),
        `The answer above is a paraphrase of ${path} and may be inaccurate; check what you repeat against the file:`,
        ...where.map((l) => l.trim()),
      ],
    };
  });

  // WebSearch answers as before, with Exa's results beside it
  on('tool.call', { tool: 'WebSearch' }, async ($, e, next) => {
    const domains = [...(e.allowed_domains ?? []), ...(e.blocked_domains ?? []).map((d) => `-${d}`)];
    const after = await beside($, e, [...domains.flatMap((d) => ['-d', d]), '-s', e.query]);
    const r = await next(e);
    if (r.result === undefined || r.isError) return r;
    const text = await after();
    if (!text) return r;
    return { ...r, context: [...(r.context ?? []), `Exa, for the same query:\n\n${text}`] };
  });
};
