# quarry

Claude Code's WebFetch gives the model a small model's summary. Its mistakes kept annoying me enough to build a replacement. quarry gives models an efficient way to read the results themeslves. Personal tool I have used for a while that I only found interesting enough to publish now with claude mods and jev; nothing polished or guaranteed.

- **WebFetch**: saves the page to `/tmp/quarry` and hands the model its path, with the line ranges [Jev](https://typesafe.ai) scores as answering the prompt. Falls back gracefully if jev cannot be reached.
- **WebSearch**: adds [Exa](https://exa.ai)'s results for the same query.

```sh
go install github.com/jackboykin/quarry@latest
claude plugin marketplace add jackboykin/quarry
claude plugin install quarry@quarry
```

Requires `CLAUDE_CODE_ENABLE_FUNCTION_HOOKS=1`, `EXA_API_KEY` for search, and `TYPESAFE_API_KEY` for fetch line ranges. The same keys can live in `~/.config/quarry/` as `exa-api-key` and `typesafe-api-key`.
