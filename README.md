# Bellona

Bellona is a local-first workflow engine that runs existing coding agents as external processes and controls
planning, implementation, evaluation and verification. It does not replace Claude Code or Codex; it is a control
layer above them. Reasoning and semantic judgements are left to the models, while execution order, dependencies,
retries, state transitions and completion criteria are controlled by ordinary code. The engine is written in Go
with the standard library only and targets Linux.

## Status

Pre-release. The current milestone is M0, the core contracts: domain types, the Run, Phase and Attempt transition
tables, the event journal with a pure reducer and replay, manifest freezing and compatibility, a fake runtime, and
the first formats and sample tasks of the evaluation infrastructure. There is no agent adapter yet, so Bellona
cannot run an agent. The only command is `bellona version`.

See [SPEC.md](SPEC.md) for the architecture and the roadmap, and [AGENTS.md](AGENTS.md) for the package layout,
the development commands and the project rules.

## Development

Go and the other tools come from the Nix flake. Run the tests with:

```sh
nix develop --command go test -race ./...
```

In a checkout where `flake.nix` is not yet tracked by Git, use `nix develop path:. --command ...` instead.
