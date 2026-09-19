import type { Register } from 'claude-code';

const quarry = 'quarry'; // from PATH

export const register: Register = (on) => {
  // WebFetch answers as before; quarry also saves the page and has Jev point to the lines that answer the prompt
  on('tool.call', { tool: 'WebFetch' }, async ($, e, next) => {
    const [r, saved] = await Promise.all([
      next(e),
      $.process.run([quarry, '-f', e.url, e.prompt], { timeoutMs: 60_000 }).catch(() => null),
    ]);
    const [path, ...where] = saved?.exitCode === 0 ? saved.stdout.trim().split('\n') : [];
    if (!path || r.result === undefined || r.isError) return r;
    return {
      ...r,
      context: [
        ...(r.context ?? []),
        `full: ${path} — read the source before relying on the answer above`,
        ...where.map((l) => l.trim()),
      ],
    };
  });

  // WebSearch answers as before, with Exa's results beside it
  on('tool.call', { tool: 'WebSearch' }, async ($, e, next) => {
    const domains = [...(e.allowed_domains ?? []), ...(e.blocked_domains ?? []).map((d) => `-${d}`)];
    const [r, exa] = await Promise.all([
      next(e),
      $.process
        .run([quarry, ...domains.flatMap((d) => ['-d', d]), '-s', e.query], { timeoutMs: 60_000 })
        .catch(() => null),
    ]);
    const text = exa?.exitCode === 0 && exa.stdout.trim();
    if (!text || r.result === undefined) return r;
    return { ...r, context: [...(r.context ?? []), `Exa, for the same query:\n\n${text}`] };
  });
};
