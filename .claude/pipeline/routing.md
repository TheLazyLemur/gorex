# Routing

Difficulty (from rubric.md) to the model that implements the ticket.

| difficulty | model |
|---|---|
| 1-3 | sonnet |
| 4 | opus |
| 5 | fable |

Security-critical work (auth, payments, secrets, permissions, crypto) never goes below opus. Only difficulty 5 goes to fable.

## Review

Tickets at or above this difficulty open as a `Draft: ` PR and are reviewed by agents on opus before
you see them. Raise it above 5 to turn agent review off.

Review threshold: 3

## Agent limits

Every agent the pipeline spawns runs within two limits: its context in tokens, as Claude Code's status line counts
it, and its wall time in minutes. At 80% of either it is told to wrap up; at the limit every tool call but
`report_result` is refused; two model requests later it is stopped and its ticket comes to you.

Agent token limit: 500000
Agent time limit: 120
