# Go dependency management

ResMan keeps dependency inspection separate from dependency mutation. Routine
checks must not rewrite `go.mod` or `go.sum`; updates are reviewed and committed
as ordinary source changes.

## Non-mutating checks

Use the exact toolchain selected by `go.mod` and the writable project caches:

```bash
PATH=/usr/local/go/bin:/home/francesco/go/bin:/usr/bin:/bin \
GOCACHE=/home/francesco/go/cache \
GOMODCACHE=/home/francesco/go/pkg/mod \
make GO=/usr/local/go/bin/go deps-weekly
```

The available commands are:

- `deps-check`: list available module updates.
- `deps-check-json`: emit the same module data as JSON objects.
- `deps-verify`: verify downloaded modules against `go.sum`.
- `deps-vuln`: fail when the official Go scanner finds a reachable
  vulnerability; it requires `govulncheck`.
- `deps-audit`: run update discovery, module verification and the vulnerability
  scan.
- `deps-report`: write the consolidated report and its raw inputs.
- `deps-weekly`: write a consolidated report under
  `build/dependencies/deps-report.txt` together with its raw inputs.

Install the reviewed scanner version with `make deps-vuln-install`. The version
is pinned in `Makefile`; ordinary checks never install tools implicitly and
refuse an installed scanner with a different version.

The periodic report records scanner output even when vulnerabilities are found,
so it is an inspection artifact rather than a pass/fail gate. Use `deps-audit`
when a failing vulnerability gate is required.

## Controlled updates

Update one module from clean module files:

```bash
make deps-update MODULE=golang.org/x/sys
```

Update the explicit set of direct runtime dependencies:

```bash
make deps-update-core
```

Both targets refuse to start when `go.mod` or `go.sum` already contains
uncommitted changes. They run `go mod tidy` and `go mod verify`, but the resulting
diff remains for review. They do not create a commit or advance the release
identity automatically.

After reviewing the module diff and relevant upstream release notes, run:

```bash
make deps-test
```

This executes the normal quality gate followed by bounded fuzzing. Real-kernel,
SmolVM and package acceptance remain separate evidence and are required only
when the dependency change can affect those contracts.
