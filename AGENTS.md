# Repository Development Workflow

These rules apply to every non-trivial bug fix and requirement change.

## Required delivery flow

1. Create a GitHub Issue before changing code.
   - Use the bug form for defects and include reproduction steps.
   - Use the feature form for requirement changes and include acceptance criteria.
2. Create a branch from the latest `main` named `fix/issue-<number>-<slug>` or `feat/issue-<number>-<slug>`.
3. Make only changes needed by the Issue. Do not push directly to `main`.
4. Run the local quality gate before committing:

   ```bash
   test -z "$(gofmt -l .)"
   go test ./...
   go vet ./...
   go build ./...
   docker build -t model-monitor-lite:verify .
   ```

5. Stop and ask the maintainer to perform local acceptance. Do not commit, push, or open a PR until the maintainer explicitly confirms the result.
   - If the maintainer reports a problem, continue fixing it, rerun the local quality gate, and request acceptance again.
   - Do not infer approval from silence or from automated test results.
6. After explicit approval, rerun affected checks, commit, push the branch, and open a PR whose body contains `Closes #<number>`.
7. Enable squash auto-merge with branch deletion without waiting for another prompt:

   ```bash
   gh pr merge --auto --squash --delete-branch
   ```

8. Treat the change as complete only after required GitHub checks pass, the PR is merged, and the linked Issue is closed automatically.

The PR CI additionally runs the race detector and Docker image build on Ubuntu. If a required command cannot run locally, report the blocker explicitly in the PR; never silently skip validation.
