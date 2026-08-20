# Contributing to Upcontrol

Thanks for looking under the hood. Small, focused PRs land fastest; for
anything larger than a bugfix, open an issue or a Discussion first so nobody
builds two weeks of the wrong thing.

## Dev setup

Prerequisites: Go 1.26+, Node 22+, Docker with Compose v2.

```sh
# Backend — three services, one module. tsc-style: build IS the typecheck.
cd back
go build ./... && go test ./...

# Frontend — Vite dev server on :5199, Playwright against a stubbed API.
cd front
npm install
npm run dev            # http://localhost:5199
npm run typecheck
npm run test           # playwright (starts its own dev server if none runs)

# The whole product, live
cd infra
./install.sh --from-source
```

The API contract is `back/api/openapi.yaml`. Change it first, then regenerate
both sides — `go tool oapi-codegen` in `back/` (see the Makefile) and
`npm run gen:api` in `front/`. Never hand-edit `front/src/lib/api.d.ts` or
`back/gen/**`.

## Tests are the gate

- Go: `go test ./...` must pass; a new branch of logic brings a test with it.
- Front: `npm run test` must pass; a new screen pins its exact copy in
  `front/e2e/`.
- CI runs both plus a compose/installer syntax check on every PR, with no
  secrets — fork PRs run the same jobs.

## Style

- Match what surrounds your change; the codebase's conventions win over
  personal ones.
- Code comments in English, and only where the code cannot state the
  constraint itself.
- UI copy: complete sentences; status is never colour alone; a failed read
  and an empty list are different facts and may never render the same.

## CLA

First-time contributors are asked to sign the [CLA](CLA.md) — the
cla-assistant bot comments on your first PR with a link, and signing is one
click with your GitHub account. It exists so the project can keep offering
the code under AGPL while running the hosted service; you keep the copyright
to your contribution.

## Merging

PRs are **squash-merged**: one commit per PR on `master`, titled from the PR
title. Keep the PR title in the imperative ("fix the retry backoff", not
"fixed"/"fixes") — it becomes history.
