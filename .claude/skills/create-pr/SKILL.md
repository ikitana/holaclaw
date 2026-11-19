---
name: create-pr
description: Open (or reuse) the pull request for the current branch of holaclaw, with a Conventional Commit title, a short body written for reviewers, an honest account of what was tested, a link to the issue it closes and a review request. Use when asked to open, create, publish or send a PR or pull request.
argument-hint: "[issue <#N>] [draft]"
allowed-tools: Bash(git status:*), Bash(git diff:*), Bash(git log:*), Bash(git branch:*), Bash(git fetch:*), Bash(git switch -c:*), Bash(git add:*), Bash(git commit:*), Bash(git push -u origin:*), Bash(nix develop -c just:*), Bash(gh pr list:*), Bash(gh pr view:*), Bash(gh pr create:*), Bash(gh pr edit:*), Bash(gh issue view:*), Bash(gh api repos/*/collaborators*)
---

# Create PR

Publish the work on the current branch as a pull request against `main`: $ARGUMENTS

Every PR in this repository has the same shape, whoever opens it: the same branch naming, the same title format, the same body sections in the same order. Follow the formats below exactly; don't adapt them to the change.

## Rules

- **One PR per branch.** If the branch already has a PR, reuse it; never open a second one.
- **Never from `main`.** If the work is on `main`, move it to a new branch first: `<type>/<short-name>`, with the type of the PR title (`feat/tasks-list-cli`, `docs/agents-commit-conventions`).
- **The title is a Conventional Commit subject**: `<type>(<scope>): <description>`. Imperative, lowercase, no final period, under 72 characters. Types: `feat`, `fix`, `docs`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`. The scope is optional and names the part touched (`cli`, `api`, `agent`, `store`, `docs`, ...). A breaking change gets `!` before the colon. A squash merge of a multi-commit PR lands the title on `main` as the commit subject.
- **Every commit is a Conventional Commit too, in the same format as the title.** This is a precondition: a branch with a commit that isn't one gets no PR until it is fixed (step 1). A rebase merge lands each one on `main` as it is, and a squash merge of a single-commit PR uses that commit's message, not the PR's.
- **The body is for reviewers**: short, in plain words, with code identifiers in backticks. It doesn't repeat the diff.
- **Say what was tested, truthfully.** If a check wasn't run, or failed, the body says so and why. Never write "passes" for something that didn't run on the pushed commit.
- **Link the issue** with `Closes #N` when the PR finishes one, `Part of #N` when it doesn't. A PR with no issue is fine; don't invent one.
- **No AI attribution**: no `Co-Authored-By` trailer for an assistant on any commit, and no "Generated with ..." footer or other AI credit in the title or body (`AGENTS.md`).
- **No private data** in the title, body or commits: no `.env` values, credentials, real conversations or customer names.
- Don't hard-wrap the body.
- Don't force-push, rewrite history or merge from this skill.

## 1. Inspect

```sh
git status --short
git fetch origin main
git log --oneline origin/main..HEAD
git diff --stat origin/main...HEAD
```

- No commits and no changes against `origin/main`: stop and say so.
- Uncommitted changes: show them and ask whether they belong to this PR. Commit the ones that do with a Conventional Commit message, adding the files by name. Leave the rest alone.
- Every commit ahead of `origin/main` must be a Conventional Commit. This command prints the ones that aren't, and must print nothing:

  ```sh
  git log --format='%h %s' origin/main..HEAD | grep -vE '^[0-9a-f]+ (feat|fix|docs|refactor|perf|test|build|ci|chore)(\([a-z0-9/_-]+\))?!?: [^A-Z ].{0,70}[^. ]$'
  ```

  It checks the type, the optional scope, the `: ` separator, a description that doesn't start with a capital or end with a period, and the length. Merge commits fail it too: rebase the branch on `origin/main` instead of merging `main` into it. Also read the full messages (`git log origin/main..HEAD`) for AI attribution trailers.
- **If any commit fails, no PR is opened, not even a draft.** Stop, show the failing commits and a corrected subject for each. Fixing them means rewriting history, which is the user's call: reword them only if the user says so, and run this step again from the start afterwards.
- Read the whole diff (`git diff origin/main...HEAD`), not only the stat: the body describes what changed, and you can't describe what you haven't read.
- If an issue was named, read it (`gh issue view <N>`) and check the diff does what it asks. Tell the user about anything it asks for that the diff doesn't do.

## 2. Check

Run, on the commit that will be pushed:

```sh
nix develop -c just check
nix develop -c just docs-check
nix develop -c just gen-check
```

`check` builds, vets, lints and runs the full test suite. `docs-check` and `gen-check` fail when `docs/reference` or `internal/api/gen` is stale: regenerate with `just docs` or `just gen`, commit the result, and run the checks again.

If something fails, stop and report the failure. Open the PR anyway only if the user says so, as a draft, with the failure written in the body. If the user asks to skip the checks, the body says they weren't run.

## 3. Draft

**Title**: per the rule above. Describe the change, not the issue: `fix(access): expire requests while the daemon runs`, not `Fix #42`. For a single-commit PR, use that commit's subject.

**Body**: exactly these sections, with these headings, in this order. No others, no text before `## Summary`, and no footer.

```markdown
## Summary

What changed and why, in one to three short paragraphs.

- `path/to/file.go`: only what helps a reviewer find their way in the diff.

Closes #N

## Testing

- `nix develop -c just check`: passes | fails: <what> | not run: <reason>
- The new tests and the cases they cover, failure paths included.
- Manual checks, if any.

## Notes

- Decisions the issue left open and the reading taken, risks, follow-ups.
```

- **Summary**: the paragraphs first, then the file bullets (`` `path`: what changed ``), then the issue line on its own.
- **Testing**: always a list, and its first bullet is always the `just check` line in one of the three forms above.
- **Notes**: always a list.

Drop the `Closes` line when there is no issue, and the Notes section when there is nothing to say; nothing else is optional. Mention in the Summary any change to a shared contract (`AGENTS.md` lists them), to the config schema, the CLI, the API spec or the event types, and any new dependency with its reason.

## 4. Review

Show the user the branch, the commits, the title, the full body, whether it is a draft, and the reviewer. Wait for their go-ahead. Apply their edits and show it again if they change anything substantial.

## 5. Create

```sh
git push -u origin <branch>
gh pr list --head <branch> --state all --json number,state,url
```

- An open PR exists: reuse it. Update its title and body with `gh pr edit <pr> --title ... --body-file ...` if they no longer describe the branch or don't follow the formats above, and say that you did.
- A merged or closed PR exists for the branch: stop and ask.
- None: write the body to a file in the scratchpad directory, then

```sh
gh pr create --base main --head <branch> --title "<title>" --body-file <scratchpad>/pr.md [--draft]
```

If the push is rejected because the remote branch has diverged, stop and report; don't force it.

## 6. Request review

The reviewers are the repository's other maintainers: everyone who can push to it, except the PR's author.

```sh
gh api 'repos/{owner}/{repo}/collaborators' --jq '.[] | select(.permissions.push) | .login'
gh pr view <pr> --json author,reviewRequests
```

Keep a reviewer who is already requested, and add the missing ones with `gh pr edit <pr> --add-reviewer <login>`. If the list leaves nobody to ask, say so and ask the user. Don't request review on a draft.

## Result

```text
<Created | Reused> PR #<pr> from `<branch>`: <title>
<url>
Closes: #<N> | none. Reviewer: <login> | none (draft).
Checks: <what ran and its result, or what did not run>.
```
