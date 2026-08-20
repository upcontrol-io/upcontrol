# Security policy

## Reporting a vulnerability

Please do not open a public issue for anything security-sensitive.

Email **security@upcontrol.io** with the details — what you found, how to
reproduce it, and what an attacker could do with it. You will get a human
reply within 72 hours, and a fix timeline once the report is confirmed.
GitHub's [private vulnerability reporting](https://github.com/upcontrol-io/upcontrol/security/advisories/new)
works too, if you prefer it.

Please give us reasonable time to ship a fix before public disclosure; we
credit reporters in the release notes unless you ask otherwise.

## Scope

- The services in this repository (`ucapi`, `ucworker`, `ucprobe`, the web
  app, the installer) as deployed by `infra/`.
- The published npm packages (`upcontrol`, `@upcontrol/sdk`): a secret
  reaching stdout or the wire unscrubbed is a security incident, not a bug.

## Supported versions

The latest release. Self-hosts are expected to track releases via
`./install.sh --update`.
