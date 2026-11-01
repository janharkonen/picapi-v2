---
name: update-architecture
description: Keep ARCHITECTURE.md in sync with the code and infrastructure. Use after making ANY change in this repository (code, routes, config/env vars, Dockerfile, dependencies, Railway services, buckets, domains) and before committing, to check whether ARCHITECTURE.md needs updating and update it.
---

# Keep ARCHITECTURE.md up to date

`ARCHITECTURE.md` at the repository root is the single source of truth for how
picapi is built and deployed. It must describe the system as it is after your
change, not before.

Run this check for every change, before committing.

## Steps

1. Look at what changed: `git status` and `git diff` (plus `git diff --staged`).
   Include changes made outside the repo, such as Railway services, variables,
   buckets or domains.
2. Read `ARCHITECTURE.md`.
3. For each section, ask whether the change makes it wrong or incomplete:
   - **Overview / principles**: data flow, caching, auth, transformations, state.
   - **Components**: routes, status codes, response headers, streaming behaviour.
   - **Configuration**: every environment variable the code reads, with
     required/default values. Compare against `os.Getenv` calls in the code.
   - **Deployment**: Railway services, buckets, domains, build settings.
   - **Local development**: commands still work as written.
4. Edit `ARCHITECTURE.md` so it matches reality. Keep the existing style:
   short, factual, tables for routes and variables. Remove anything that is no
   longer true; do not leave history or "previously" notes (git has history).
5. If nothing architecturally relevant changed (typo fixes, refactors with no
   behaviour change, test-only changes), make no edit and say so briefly.
6. Commit `ARCHITECTURE.md` together with the change it describes.
