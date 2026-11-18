#!/bin/sh
# The pipeline's tracker: GitHub issues on the fork, through gh's own login.
# Statuses are labels on open issues (ready, in-progress, in-review); done is
# closed as completed. tools/tracker holds its contract tests.
set -eu

REPO=${TRACKER_REPO:-TheLazyLemur/gorex}
STATUSES="ready in-progress in-review"

die() { echo "tracker.sh: $*" >&2; exit 2; }
issue_id() { case ${1:-} in '' | *[!0-9]*) die "not an issue number: '${1:-}'" ;; esac; }

# The open ready issues that one PR can finish: no epic label, no sub-issues.
# shortcut: an issue's first 50 labels and 100 blockers only, enough for a hand-made board.
# shortcut: blockers in other repos are reported by number alone, add the repo if they ever appear.
ready_issues() {
	gh api graphql --paginate -f owner="${REPO%/*}" -f name="${REPO#*/}" -f query='
query($owner: String!, $name: String!, $endCursor: String) {
  repository(owner: $owner, name: $name) {
    issues(states: OPEN, labels: ["ready"], first: 100, after: $endCursor) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number title url body
        labels(first: 50) { nodes { name } }
        subIssuesSummary { total }
        blockedBy(first: 100) { nodes { number state stateReason } }
      }
    }
  }
}' | jq -s '[.[].data.repository.issues.nodes[]
	| select((.labels.nodes | map(.name) | index("epic")) == null and .subIssuesSummary.total == 0)]'
}

cmd=${1:-}
[ $# -gt 0 ] && shift
case $cmd in
waiting)
	ready_issues | jq '[.[] | {id: .number, title, url, body}]'
	;;
done)
	gh issue list -R "$REPO" --state closed --limit 10000 --json number,stateReason |
		jq '[.[] | select(.stateReason == "COMPLETED") | {id: .number}]'
	;;
blockers)
	ready_issues | jq '[.[] | .number as $id | .blockedBy.nodes[]
		| {id: $id, blocked_by: .number, blocker_done: (.state == "CLOSED" and .stateReason == "COMPLETED")}]'
	;;
merged)
	[ -n "${1:-}" ] || die "usage: merged PR_URL"
	gh pr view "$1" --json state | jq -c '{merged: (.state == "MERGED")}'
	;;
mark-done)
	issue_id "${1:-}"
	issue=$(gh issue view "$1" -R "$REPO" --json state,stateReason)
	case $(printf '%s' "$issue" | jq -r '.state + " " + .stateReason') in
	"OPEN "*) gh issue close "$1" -R "$REPO" --reason completed ;;
	"CLOSED COMPLETED") ;;
	*) die "issue $1 is closed but not as completed: $issue" ;;
	esac
	;;
read)
	issue_id "${1:-}"
	gh issue view "$1" -R "$REPO" --json number,title,url,state,stateReason,labels,body,comments
	;;
move)
	issue_id "${1:-}"
	to=${2:-}
	case " $STATUSES done " in *" $to "*) ;; *) die "status must be one of: $STATUSES done" ;; esac
	if [ "$to" = done ]; then exec "$0" mark-done "$1"; fi
	others=
	for s in $STATUSES; do [ "$s" = "$to" ] || others=$others${others:+,}$s; done
	gh issue edit "$1" -R "$REPO" --add-label "$to" --remove-label "$others"
	;;
comment)
	issue_id "${1:-}"
	gh issue comment "$1" -R "$REPO" --body-file -
	;;
link-pr)
	issue_id "${1:-}"
	[ -n "${2:-}" ] || die "usage: link-pr ISSUE PR_URL"
	gh issue comment "$1" -R "$REPO" --body "PR: $2"
	body=$(gh pr view "$2" --json body | jq -r .body)
	if ! printf '%s\n' "$body" | grep -qx "Closes #$1"; then
		gh pr edit "$2" --body "$body

Closes #$1"
	fi
	;;
create)
	# No status label: sync pulls it in only once `move ID ready` runs, after its blockers are linked.
	draft=$(cat)
	title=$(printf '%s' "$draft" | jq -er '.title | select(type == "string" and . != "")') ||
		die 'create reads {"title", "body"} JSON on stdin, with a title'
	url=$(printf '%s' "$draft" | jq -r '.body // ""' | gh issue create -R "$REPO" --title "$title" --body-file - | tail -n 1)
	jq -n --arg url "$url" '{id: ($url | split("/") | last | tonumber), url: $url}'
	;;
link-blocker)
	issue_id "${1:-}"
	issue_id "${2:-}"
	blocker=$(gh api "repos/$REPO/issues/$2" --jq .id)
	gh api "repos/$REPO/issues/$1/dependencies/blocked_by" -F issue_id="$blocker" >/dev/null
	;;
add-sub-issue)
	issue_id "${1:-}"
	issue_id "${2:-}"
	child=$(gh api "repos/$REPO/issues/$2" --jq .id)
	gh api "repos/$REPO/issues/$1/sub_issues" -F sub_issue_id="$child" >/dev/null
	;;
*)
	die "usage: tracker.sh waiting|done|blockers|merged URL|mark-done ID|read ID|move ID STATUS|comment ID <body|link-pr ID URL|create <json|link-blocker ID BLOCKER|add-sub-issue PARENT CHILD"
	;;
esac
