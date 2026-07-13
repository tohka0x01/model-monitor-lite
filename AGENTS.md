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

5. Push the branch and open a PR whose body contains `Closes #<number>`.
6. Enable squash auto-merge with branch deletion:

   ```bash
   gh pr merge --auto --squash --delete-branch
   ```

7. Treat the change as complete only after required GitHub checks pass, the PR is merged, and the linked Issue is closed automatically.

The PR CI additionally runs the race detector and Docker image build on Ubuntu. If a required command cannot run locally, report the blocker explicitly in the PR; never silently skip validation.
