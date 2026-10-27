# quarry

Claude Code's WebFetch and similar tools give the main model a small model's summary. Its mistakes kept annoying me enough to build a replacement. quarry gives models an efficient way to read the results themselves. quarry is a personal tool I have used for a while; nothing polished or guaranteed.

- **fetch**: saves the page to disk and hands the model its path, with the line ranges [Jev](https://typesafe.ai) scores as answering the prompt. Every page is also asked of Exa, whose copy is kept when the page as served came back hollow: a login wall, a footer, a file view cut short.
- **search**: [Exa](https://exa.ai)'s results with highlights.

## Install

```sh
go install github.com/jackboykin/quarry@latest
```

A flake.nix is also available. Hosts that read Agent Skills can use [skills/quarry](skills/quarry/SKILL.md).

**Claude Code**: a plugin whose one hook puts quarry's usage in every session.

```sh
claude plugin marketplace add jackboykin/quarry
claude plugin install quarry@quarry
```

**[pi](https://github.com/badlogic/pi-mono)**: `pi install git:github.com/jackboykin/quarry`

Keys: `EXA_API_KEY` for search, `TYPESAFE_API_KEY` for fetch line ranges. They can also live in `~/.config/quarry/` as `exa-api-key` and `typesafe-api-key`.
