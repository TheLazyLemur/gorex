// Package tracker holds the contract tests of .claude/pipeline/tracker.sh,
// the pipeline's adapter to the fork's GitHub issues. A fake gh answers with
// recorded API responses, so no real issue changes.
package tracker

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// fakeGh puts a gh on PATH that prints responses[its arguments] and logs
// each call, then its stdin; an unknown call fails, as a broken API would.
func fakeGh(t *testing.T, responses map[string]string) (calls func() string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("tracker.sh is a POSIX shell script")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"$*\" >> " + dir + "/calls\ncat >> " + dir + "/calls\ncase \"$*\" in\n"
	for args, out := range responses {
		// Quote all but the * of the pattern.
		pattern := "'" + strings.ReplaceAll(args, "*", "'*'") + "'"
		script += pattern + ") cat <<'EOF'\n" + out + "\nEOF\n;;\n"
	}
	script += "*) echo \"unexpected gh $*\" >&2; exit 1;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TRACKER_REPO", "owner/repo")
	return func() string { b, _ := os.ReadFile(filepath.Join(dir, "calls")); return string(b) }
}

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return runWith(t, "", args...)
}

func runWith(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("../../.claude/pipeline/tracker.sh", args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	return string(out), err
}

// equalJSON fails unless got is the JSON value want.
func equalJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("output %q is not JSON: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// Two pages of open ready issues: a work item blocked by a done and an open
// issue, an epic, and a parent of sub-issues.
const readyPages = `{"data":{"repository":{"issues":{"nodes":[
 {"number":1,"title":"Per-block font size","url":"https://github.com/owner/repo/issues/1","body":"Do it.\n\n- [ ] works",
  "labels":{"nodes":[{"name":"ready"}]},"subIssuesSummary":{"total":0},
  "blockedBy":{"nodes":[{"number":7,"state":"CLOSED","stateReason":"COMPLETED"},{"number":8,"state":"OPEN","stateReason":null}]}},
 {"number":2,"title":"Phase 1","url":"https://github.com/owner/repo/issues/2","body":"",
  "labels":{"nodes":[{"name":"ready"},{"name":"epic"}]},"subIssuesSummary":{"total":0},"blockedBy":{"nodes":[{"number":9,"state":"OPEN","stateReason":null}]}}]}}}}
{"data":{"repository":{"issues":{"nodes":[
 {"number":3,"title":"Parent","url":"https://github.com/owner/repo/issues/3","body":"",
  "labels":{"nodes":[{"name":"ready"}]},"subIssuesSummary":{"total":2},"blockedBy":{"nodes":[]}},
 {"number":4,"title":"Unblocked","url":"https://github.com/owner/repo/issues/4","body":"b",
  "labels":{"nodes":[{"name":"ready"}]},"subIssuesSummary":{"total":0},"blockedBy":{"nodes":[]}}]}}}}`

func TestWaiting(t *testing.T) {
	fakeGh(t, map[string]string{"api graphql --paginate *": readyPages})
	out, err := run(t, "waiting")
	if err != nil {
		t.Fatal(err)
	}
	// Both pages, without the epic or the parent.
	equalJSON(t, out, `[
	 {"id":1,"title":"Per-block font size","url":"https://github.com/owner/repo/issues/1","body":"Do it.\n\n- [ ] works"},
	 {"id":4,"title":"Unblocked","url":"https://github.com/owner/repo/issues/4","body":"b"}]`)
}

func TestBlockers(t *testing.T) {
	fakeGh(t, map[string]string{"api graphql --paginate *": readyPages})
	out, err := run(t, "blockers")
	if err != nil {
		t.Fatal(err)
	}
	equalJSON(t, out, `[
	 {"id":1,"blocked_by":7,"blocker_done":true},
	 {"id":1,"blocked_by":8,"blocker_done":false}]`)
}

func TestDone(t *testing.T) {
	fakeGh(t, map[string]string{"issue list -R owner/repo --state closed *": `[
	 {"number":5,"stateReason":"COMPLETED"},{"number":6,"stateReason":"NOT_PLANNED"}]`})
	out, err := run(t, "done")
	if err != nil {
		t.Fatal(err)
	}
	// Closed as not planned isn't done.
	equalJSON(t, out, `[{"id":5}]`)
}

func TestMerged(t *testing.T) {
	fakeGh(t, map[string]string{
		"pr view https://github.com/owner/repo/pull/1 --json state": `{"state":"MERGED"}`,
		"pr view https://github.com/owner/repo/pull/2 --json state": `{"state":"OPEN"}`,
	})
	out, err := run(t, "merged", "https://github.com/owner/repo/pull/1")
	if err != nil {
		t.Fatal(err)
	}
	equalJSON(t, out, `{"merged":true}`)
	out, err = run(t, "merged", "https://github.com/owner/repo/pull/2")
	if err != nil {
		t.Fatal(err)
	}
	equalJSON(t, out, `{"merged":false}`)
}

func TestMarkDone(t *testing.T) {
	calls := fakeGh(t, map[string]string{
		"issue view 1 -R owner/repo --json state,stateReason": `{"state":"OPEN","stateReason":null}`,
		"issue view 2 -R owner/repo --json state,stateReason": `{"state":"CLOSED","stateReason":"COMPLETED"}`,
		"issue view 3 -R owner/repo --json state,stateReason": `{"state":"CLOSED","stateReason":"NOT_PLANNED"}`,
		"issue close 1 -R owner/repo --reason completed":      ``,
	})
	if _, err := run(t, "mark-done", "1"); err != nil {
		t.Fatalf("open issue: %v", err)
	}
	// Already done succeeds without closing it again.
	if _, err := run(t, "mark-done", "2"); err != nil {
		t.Fatalf("already done: %v", err)
	}
	if strings.Contains(calls(), "issue close 2") {
		t.Fatal("closed an issue already done")
	}
	// Closed as not planned is a decision a human made: fail loudly.
	if _, err := run(t, "mark-done", "3"); err == nil {
		t.Fatal("marked an issue closed as not planned done")
	}
	// A bad id fails before reaching gh, so it can't be spliced into a call.
	before := calls()
	for _, id := range []string{"", "abc", "1; rm -rf /", "-1"} {
		if _, err := run(t, "mark-done", id); err == nil {
			t.Fatalf("mark-done %q succeeded", id)
		}
	}
	if calls() != before {
		t.Fatal("a bad id reached gh")
	}
}

func TestMove(t *testing.T) {
	calls := fakeGh(t, map[string]string{
		"issue edit 1 -R owner/repo --add-label in-review --remove-label ready,in-progress": ``,
	})
	if _, err := run(t, "move", "1", "in-review"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "move", "1", "review"); err == nil {
		t.Fatal("moved to an unknown status")
	}
	if n := strings.Count(calls(), "\n"); n != 1 {
		t.Fatalf("%d gh calls, want 1", n)
	}
}

func TestLinkPR(t *testing.T) {
	pr := "https://github.com/owner/repo/pull/9"
	calls := fakeGh(t, map[string]string{
		"issue comment 1 -R owner/repo --body PR: " + pr: ``,
		"issue comment 2 -R owner/repo --body PR: " + pr: ``,
		"pr view " + pr + " --json body":                 `{"body":"Adds it.\n\nCloses #2"}`,
		"pr edit " + pr + " --body *":                    ``,
	})
	if _, err := run(t, "link-pr", "1", pr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(calls(), "pr edit "+pr+" --body Adds it.\n\nCloses #2\n\nCloses #1") {
		t.Fatalf("PR body not given Closes #1:\n%s", calls())
	}
	// Already linked: the body is left alone.
	before := strings.Count(calls(), "pr edit")
	if _, err := run(t, "link-pr", "2", pr); err != nil {
		t.Fatal(err)
	}
	if strings.Count(calls(), "pr edit") != before {
		t.Fatal("edited a PR body that already closes the issue")
	}
}

func TestCreate(t *testing.T) {
	calls := fakeGh(t, map[string]string{
		"issue create -R owner/repo --title Per-block font size --body-file -": "https://github.com/owner/repo/issues/12",
	})
	out, err := runWith(t, `{"title":"Per-block font size","body":"Do it.\n\n- [ ] works"}`, "create")
	if err != nil {
		t.Fatal(err)
	}
	equalJSON(t, out, `{"id":12,"url":"https://github.com/owner/repo/issues/12"}`)
	// The body reaches gh, and no status label: sync must not pull it in before its blockers are linked.
	if !strings.Contains(calls(), "--body-file -\nDo it.\n\n- [ ] works") {
		t.Fatalf("body not passed to gh:\n%s", calls())
	}
	// No title fails before reaching gh.
	before := calls()
	for _, draft := range []string{`{"body":"b"}`, `{"title":"","body":"b"}`, `not json`} {
		if _, err := runWith(t, draft, "create"); err == nil {
			t.Fatalf("create %q succeeded", draft)
		}
	}
	if calls() != before {
		t.Fatal("a bad draft reached gh")
	}
}

func TestLinkBlocker(t *testing.T) {
	calls := fakeGh(t, map[string]string{
		"api repos/owner/repo/issues/7 --jq .id":                                  "3007",
		"api repos/owner/repo/issues/12/dependencies/blocked_by -F issue_id=3007": `{}`,
	})
	if _, err := run(t, "link-blocker", "12", "7"); err != nil {
		t.Fatalf("%v\n%s", err, calls())
	}
	before := calls()
	if _, err := run(t, "link-blocker", "12", "x"); err == nil {
		t.Fatal("linked a bad blocker id")
	}
	if calls() != before {
		t.Fatal("a bad id reached gh")
	}
}

func TestAddSubIssue(t *testing.T) {
	calls := fakeGh(t, map[string]string{
		"api repos/owner/repo/issues/12 --jq .id":                       "3012",
		"api repos/owner/repo/issues/5/sub_issues -F sub_issue_id=3012": `{}`,
	})
	if _, err := run(t, "add-sub-issue", "5", "12"); err != nil {
		t.Fatalf("%v\n%s", err, calls())
	}
}
