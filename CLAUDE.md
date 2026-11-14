# ghostlap

Build spec: [docs/PRD.md](docs/PRD.md). Follow section 0: measure never guess, stdlib-first Go, purpose-built storage engine.

- Test: `go test -race ./...`
- Vet: `go vet ./...`
- Data lives in `data/` (gitignored): `data/raw` cache, `data/segments`, `data/MANIFEST`.
