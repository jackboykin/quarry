// Loads quarry.ts through pi's own jiti with a stubbed ExtensionAPI and runs both tools.
// nix shell nixpkgs#pi-coding-agent nixpkgs#nodejs -c node pi/smoke.mjs
import { execFile, execSync } from 'node:child_process';
import { realpathSync } from 'node:fs';

const mods = realpathSync(execSync('which pi').toString().trim()).replace(
  /\/bin\/[^/]+$/,
  '/lib/node_modules/pi-monorepo/node_modules/',
);
const { createJiti } = await import(`${mods}jiti/lib/jiti.mjs`);
const jiti = createJiti(mods, { alias: { typebox: `${mods}typebox/build/index.mjs` } });
const ext = await jiti.import(new URL('./quarry.ts', import.meta.url).pathname, { default: true });

const tools = {};
ext({
  registerTool: (t) => (tools[t.name] = t),
  exec: (cmd, args, o) =>
    new Promise((res) =>
      execFile(cmd, args, o, (e, stdout, stderr) =>
        res({ code: e?.code ?? 0, stdout, stderr, killed: !!e?.killed }),
      ),
    ),
});

for (const [n, t] of Object.entries(tools)) console.log(n, JSON.stringify(t.parameters));
console.log(
  (await tools.web_search.execute('1', { query: 'html-to-markdown go library', n: 2 })).content[0].text,
);
console.log(
  (
    await tools.web_fetch.execute('2', {
      urls: ['https://go.dev/doc/effective_go'],
      question: 'how are errors handled',
    })
  ).content[0].text,
);
