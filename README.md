# quarry

Claude Code's WebFetch and similar tools give the main model a small model's summary. Its mistakes kept annoying me enough to build a replacement. quarry gives models an efficient way to read the results themselves. quarry is a personal tool I have used for a while that I only found interesting enough to publish now with claude mods and jev; nothing polished or guaranteed.

- **fetch**: saves the page to disk and hands the model its path, with the line ranges [Jev](https://typesafe.ai) scores as answering the prompt. Falls back gracefully if jev cannot be reached.
- **search**: [Exa](https://exa.ai)'s results with highlights.

## Install

The binary, then whichever host you use. Any other host that reads Agent Skills can use [skills/quarry](skills/quarry/SKILL.md).

```sh
go install github.com/jackboykin/quarry@latest
```

A flake.nix is also available.

Keys: `EXA_API_KEY` for search, `TYPESAFE_API_KEY` for fetch line ranges. They can also live in `~/.config/quarry/` as `exa-api-key` and `typesafe-api-key`.

**Claude Code** — augments the built-in WebFetch and WebSearch. Needs `CLAUDE_CODE_ENABLE_FUNCTION_HOOKS=1` and a recent version.

```sh
claude plugin marketplace add jackboykin/quarry
claude plugin install quarry@quarry
```

**[pi](https://github.com/badlogic/pi-mono)**

```sh
pi install git:github.com/jackboykin/quarry
```
