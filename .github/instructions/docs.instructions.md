---
applyTo: "docs/**,README.md"
paths:
  - "docs/**"
  - "README.md"
description: "Rules for the documentation: numbered pages, index, the in-app copy, accuracy, markdownlint."
---

# Documentation instructions

- `docs/NN-name.md` are numbered pages in reading order; `docs/index.md` lists every page; `README.md` links the most important ones. A new page is added to the index and, if relevant, to the README table.
- **Document what you ran.** Commands, flags and output you write must match the code: check flag names against `cmd/`, defaults against the code, and run the commands where you can. Do not describe planned behaviour as existing behaviour.
- The in-app documentation (`pkg/frontend/templates/docs.html`) mirrors these pages. Update both; `docs_test.go` checks that internal anchors resolve.
- Write for the reader of that page: users on the user pages, operators on the deployment pages (`03`, `15`), developers on `13`. Short sentences, concrete examples, one idea per paragraph. State limits and trade-offs honestly (`14-limitations-and-security.md`).
- Flags and environment variables go in the tables of `02-configuration.md`; commands in `05-commands.md`; the two-process deployment in `15-split-deployment.md`; the public API in `11-api.md` (the internal API is not documented as an interface).
- Markdown style: ATX headings, fenced code blocks with a language, blank lines around lists and fences, no trailing spaces. `markdownlint-cli2` (default rules) is the reference; the long table rows already in the pages (MD013, MD060) are an accepted exception, new prose should be clean.
- Do not paste secrets, real hostnames or real addresses; use `example.com`, `203.0.113.0/24` and placeholders.
