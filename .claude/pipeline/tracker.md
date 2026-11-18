# Tracker: GitHub issues on TheLazyLemur/gorex

Every command goes through `.claude/pipeline/tracker.sh`, which calls `gh` with its own login (`gh auth status`;
no token is stored here). Its contract tests are `go test ./tools/tracker/`. Run everything from the project root.

## Status mapping

GitHub issues have no workflow statuses, so the open ones carry exactly one status label.

| Pipeline meaning | On GitHub |
|---|---|
| Waiting to be worked (sync pulls in as `new`) | open, label `ready` |
| Implementer starts | open, label `in-progress` |
| PR open | open, label `in-review` |
| Done | closed as completed |

Closed as not planned is not done: a ticket blocked by one stays blocked, and `mark-done` refuses it.
Epics (label `epic`) and issues with sub-issues are never pulled in; blockers are GitHub's own "blocked by" links.

## List the tickets waiting to be worked

```sh
.claude/pipeline/tracker.sh waiting
```

## List the tickets that are done

```sh
.claude/pipeline/tracker.sh done
```

## List the blockers

```sh
.claude/pipeline/tracker.sh blockers
```

## Check whether a PR is merged

```sh
.claude/pipeline/tracker.sh merged "$1"
```

## Mark a ticket done

```sh
.claude/pipeline/tracker.sh mark-done "$1"
```

## Read one ticket in full

Prints the issue as JSON: title, body (description and acceptance criteria), labels, state and every comment.

```sh
.claude/pipeline/tracker.sh read 12
```

## Move a ticket's status

The status is one of `ready`, `in-progress`, `in-review` or `done`. A label status replaces the other two
labels; `done` closes the issue as completed.

```sh
.claude/pipeline/tracker.sh move 12 in-progress
```

## Add a comment to a ticket

The comment is Markdown read from stdin, so it needs no shell quoting.

```sh
.claude/pipeline/tracker.sh comment 12 <<'EOF'
Comment text.
EOF
```

## Link a PR to a ticket

Comments the PR's URL on the issue and appends `Closes #12` to the PR body unless it is already there.

```sh
.claude/pipeline/tracker.sh link-pr 12 https://github.com/TheLazyLemur/gorex/pull/34
```

## Create a ticket

Reads `{"title", "body"}` JSON on stdin, `body` Markdown, and prints `{"id", "url"}`. The issue gets no status
label, so sync leaves it alone until it is moved to `ready`: link its blockers first.

```sh
jq -n --arg title 'Title' --arg body 'Body.' '{title: $title, body: $body}' | .claude/pipeline/tracker.sh create
```

## Link a blocker

Records that ticket `$1` is blocked by ticket `$2`, as GitHub's own "blocked by" link.

```sh
.claude/pipeline/tracker.sh link-blocker 12 7
```

## Make a ticket a sub-issue

Makes ticket `$2` a sub-issue of ticket `$1`. A parent with sub-issues is never pulled in, so its children are
worked instead.

```sh
.claude/pipeline/tracker.sh add-sub-issue 5 12
```
