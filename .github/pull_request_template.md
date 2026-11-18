## What and why

<!-- One change per pull request. Link the issue if there is one. -->

## How it was tested

- [ ] `go test ./...` and `go vet ./...` pass (`GOOS=windows go vet ./...` too)
- [ ] New behaviour has a test
- [ ] Tried in the UI against a non-production cluster (say which: kind, minikube, …)

## Checklist

- [ ] Respects read-only mode, production-like contexts and the audit log (if it changes clusters)
- [ ] No Secret values, personal data or company details in code, tests or docs
- [ ] README / help / palette updated if behaviour changed
