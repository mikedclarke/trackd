# Contributing

Thanks for taking an interest. trackd is a small, opinionated tool with one maintainer, so the process is light but the bar for changes to the data path is high.

## Before you write code

- Bugs and questions: open an issue. A failing test or a reproducible command sequence is the most useful thing you can include.
- Features and larger changes: open an issue first and describe the problem you are solving. Agreeing on the approach before the pull request saves everyone a rewrite.
- Small fixes (typos, a wrong error message, a missing flag in the help text) can go straight to a pull request.

## The rules that will not change

These are the constraints the project exists to uphold. A pull request that weakens one will not be merged, however tidy.

- No hard deletes. Rows are archived, canceled or revoked, never destroyed.
- Descriptions are append-only unless the caller asks for an overwrite in so many words, and then only with an admin token.
- Every mutation records an audit event with before and after state.
- The SQLite durability pragmas stay as they are.
- Every list response is an envelope, every error carries a machine-readable code, and the CLI exit codes are stable. Scripts depend on them.
- Pure Go, no CGO, minimal dependencies. Justify any new one.

## Working on the code

```sh
make build      # bin/trackd, version stamped from git
make test       # go test -race ./...
make lint       # gofmt, go vet, golangci-lint when installed
```

Every storage change ships with tests. Prefer table-driven tests. Client and CLI tests speak the REST contract by hand against an httptest server.

Commits use conventional prefixes (`feat:`, `fix:`, `chore:`, `docs:`) in the imperative mood. No emoji, no AI-attribution trailers.

Prose, comments and user-facing strings use plain punctuation: commas, colons and full stops, no em dashes.
