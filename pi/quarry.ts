// pi extension: web_search and web_fetch via the quarry binary on PATH,
// installed as a pi package through the root package.json.
import type { ExtensionAPI } from '@earendil-works/pi-coding-agent';
import { Type } from 'typebox';

export default function (pi: ExtensionAPI) {
  const run = async (args: string[], signal?: AbortSignal) => {
    const r = await pi.exec('quarry', args, { signal, timeout: 60_000 });
    // pi flags a tool error only when execute throws
    if (r.code !== 0) throw new Error(r.stderr.trim() || `quarry exited ${r.code}`);
    return { content: [{ type: 'text' as const, text: r.stdout.trim() }], details: {} };
  };

  pi.registerTool({
    name: 'web_search',
    label: 'Web Search',
    description: 'Search the web with Exa. Returns numbered results with highlights.',
    parameters: Type.Object({
      query: Type.String(),
      n: Type.Optional(Type.Number({ description: 'number of results, default 8' })),
      domains: Type.Optional(
        Type.Array(Type.String(), { description: 'only these hosts; prefix with - to exclude' }),
      ),
    }),
    execute: (_id, p, signal) =>
      run(
        [...(p.domains ?? []).flatMap((d) => ['-d', d]), ...(p.n ? ['-n', String(p.n)] : []), '-s', p.query],
        signal,
      ),
  });

  pi.registerTool({
    name: 'web_fetch',
    label: 'Web Fetch',
    description:
      'Fetch pages to disk and return each saved path and size. With a question, also the line ranges likely to answer it. Read the file to see the content.',
    parameters: Type.Object({
      urls: Type.Array(Type.String()),
      question: Type.Optional(Type.String()),
    }),
    execute: (_id, p, signal) => run(['-f', ...p.urls, ...(p.question ? [p.question] : [])], signal),
  });
}
