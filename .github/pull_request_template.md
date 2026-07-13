Closes #

## Change

<!-- Describe the root-cause fix or requirement change. -->

## Validation

- [ ] `test -z "$(gofmt -l .)"`
- [ ] `go test ./...`
- [ ] `go vet ./...`
- [ ] `go build ./...`
- [ ] `docker build -t model-monitor-lite:verify .`
- [ ] Maintainer explicitly confirmed local acceptance before push

## Risk

<!-- State deployment/runtime risks and rollback considerations. -->
