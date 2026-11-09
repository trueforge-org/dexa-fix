# dexa-fix instructions for Claude Code

These are the dexa-fix-specific parts of the shared `CLAUDE.md`, which is managed in `trueforge-org/.github`. The canonical, tool-agnostic rules are in `AGENTS.md`, which `CLAUDE.md` imports.

- The area-specific rules live in `.github/instructions/` and are symlinked into `.claude/rules/`, so Claude Code loads each one only when it works on files matching its `paths:` frontmatter. Update those files (and `AGENTS.md`), not the shared `CLAUDE.md`. When you add one, add the symlink too; `guidance_test.go` fails otherwise.
- Test, lint and validation runs go to `haiku-triage`: `go test` (all three modes in `AGENTS.md`: plain, `DEXAFIX_TEST_SPLIT=1`, PostgreSQL), `go vet`, `gofmt -l`, `govulncheck`, `markdownlint`. Test output is noisy JSON log lines, so ask the agent to report only `FAIL`/`panic` lines and the first error.
- Documentation and example files go to `haiku-docs` when they are plain writing. Docs that depend on how the code behaves (flags, deployment steps, security model) you write yourself, from the code, and check the commands you document.
- A rename or mechanical change across many files goes to `haiku-bulk-edit` (for example moving a package: import paths, docs mentions).
- The Docker daemon may run on another host than your shell. Bind-mounted paths from your shell are then invisible to it; see [`deploy.instructions.md`](../../.github/instructions/deploy.instructions.md) for how to test a Compose change anyway.
