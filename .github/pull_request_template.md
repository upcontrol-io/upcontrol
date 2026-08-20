<!-- The PR title becomes the squash commit on master — write it in the
     imperative ("fix the retry backoff"). -->

## What and why

<!-- What changes, and the problem it solves. Link the issue if one exists. -->

## How it was verified

<!-- The commands you ran and their outcome: `go test ./...`, `npm run test`,
     a live-stack check where the change touches the contract. "CI will tell
     us" is not verification. -->

- [ ] Tests pass locally (`go test ./...` and/or `npm run test`)
- [ ] New logic carries a test that fails without the change
- [ ] Contract untouched, or `openapi.yaml` changed FIRST and both sides regenerated
