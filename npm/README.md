# cc-connect (llmw fork)

Fork of [cc-connect](https://github.com/chenhg5/cc-connect) with first-class support for the
[llmw](https://github.com/yzr95924/llmw-workspace-cli) personal-wiki CLI as an agent backend —
chat with your llmw wikis from Telegram / DingTalk / WeChat / and every other platform cc-connect bridges.

The binary is `llmw-connect` (renamed in v1.5.0-llmw.4 so it can coexist with an
upstream `cc-connect` install on the same machine) — same engine, same platforms,
plus the `llmw` agent (`/llmw list` / `/llmw enter <wiki>` / `/llmw status` / `/llmw stop`).

## Install

```bash
npm install -g @yzr95924/llmw-connect
```

Binaries are downloaded from [GitHub Releases](https://github.com/yzr95924/llmw-cc-connect/releases)
at install time (same wrapper mechanism as upstream).

## Usage

```bash
# Create config
llmw-connect --version

# Edit config.toml, then run
llmw-connect
llmw-connect -config /path/to/config.toml
```

To use the llmw agent, set in your project config:

```toml
agent.type = "llmw"
agent.options.backend = "opencode"   # only supported value; may be omitted
```

## Versioning

Versions follow upstream: `vX.Y.Z-llmw.N` = upstream `vX.Y.Z` plus N fork releases on top.
This package's `package.json` is kept byte-identical to upstream in git; the published name
and version are injected by CI from the release tag. Do not edit `npm/package.json` on this branch.

## Documentation

- Fork: https://github.com/yzr95924/llmw-cc-connect
- Upstream docs: https://github.com/chenhg5/cc-connect
