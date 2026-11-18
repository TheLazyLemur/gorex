---
name: feed-pipeline
description: Break a plan, spec, issue, chat or the current conversation into tracer-bullet tickets that each pass the pipeline's triage, with their blocking edges, and file them on the tracker as ready. Use when the user wants work fed to the pipeline.
disable-model-invocation: true
---

<!-- Adapted from mattpocock/skills to-tickets (MIT, Copyright (c) 2026 Matt Pocock). -->

# Feed the pipeline

Break a source of work into **tickets**: tracer-bullet vertical slices, each declaring the tickets that **block**
it, filed so the pipeline's sync pulls them in and triage finds them ready.

The source can be anything: a spec or plan file, a tracker issue, a URL, pasted notes, or this conversation. Nothing
below depends on which.

Run every command from the project root. The tracker's commands are the sections of
`.claude/pipeline/tracker.md`; follow it, not your memory of the tracker.

## Process

### 1. Gather context

Work from whatever is already in the conversation. If the user passes a reference, read it in full: a file path with
Read, a tracker issue with `## Read one ticket in full` (its body and every comment), a URL by fetching it.

Read `.claude/pipeline/readiness.md` and `.claude/pipeline/rubric.md`: triage judges every ticket against them.

### 2. Explore the codebase

Find out what the source asks for that already exists, and leave it out. Run `## List the tickets waiting to be
worked` and leave out work already ticketed. Use the project's vocabulary in titles and descriptions, and respect the
decisions recorded in its docs and ADRs for the area.

Look for prefactoring that would make the implementation easier. "Make the change easy, then make the easy change."

### 3. Draft vertical slices

Break the work into **tracer bullet** tickets.

<vertical-slice-rules>

- Each slice cuts a narrow but COMPLETE path through every layer it touches, tests included: vertical, NOT a
  horizontal slice of one layer
- A completed slice is demoable or verifiable on its own
- Each slice passes every check in `readiness.md`: it says what changes and why, its acceptance criteria are checkable
  by a test or a reviewer, it leaves no question or decision to a human, and one PR finishes it
- Any prefactoring is its own ticket, done first

</vertical-slice-rules>

An open design choice is not filed open: settle it with the user in step 4 and write the decision into the ticket.
A ticket left with one would score 5 on the rubric and come back from triage as a question.

Give each ticket its **blocking edges**: the other tickets that must complete before it can start. A ticket with no
blockers can start immediately, and the pipeline works it in parallel with the others, so an edge is needed wherever
two tickets would otherwise conflict.

**Wide refactors are the exception to vertical slicing.** A **wide refactor** is one mechanical change (rename a
column, retype a shared symbol) whose **blast radius** fans across the whole codebase, so a single edit breaks every
call site at once and no vertical slice can land green. Sequence it as **expand–contract**. First expand: add the new
form beside the old so nothing breaks. Then migrate the call sites in batches sized by blast radius (per package, per
directory), each batch its own ticket blocked by the expand, keeping CI green batch to batch because the old form
still exists. Finally contract: delete the old form once no caller remains, in a ticket blocked by every migrate
batch. Every pipeline ticket lands as its own PR on the default branch, so each batch must stay green alone; if one
cannot, it is too big: split it further.

### 4. Quiz the user

Present the proposed breakdown as a numbered list. For each ticket, show:

- **Title**: short descriptive name
- **Blocked by**: which other tickets (if any) must complete first
- **What it delivers**: the end-to-end behaviour this ticket makes work

Ask the user:

- Does the granularity feel right? (too coarse / too fine)
- Are the blocking edges correct: does each ticket only depend on tickets that genuinely gate it?
- Should any tickets be merged or split further?
- Any open decision from step 3, one at a time

Iterate until the user approves the breakdown. File nothing before that: filing publishes it.

### 5. File the tickets

File the approved tickets in dependency order (blockers first), so each edge can name a real ticket id:

1. Create each ticket with `## Create a ticket`, its body from the template below. Note the id it prints.
2. Link every blocking edge with `## Link a blocker`.
3. If the source was a tracker issue, make each ticket its sub-issue with `## Make a ticket a sub-issue`. That keeps
   the parent out of the pipeline, so it is not worked whole beside its children. Do NOT close or otherwise modify
   the parent.
4. Only now move each ticket to `ready` with `## Move a ticket's status`. A ticket made ready before its blockers are
   linked can be pulled in and started early.

If a step fails, stop and tell the user which tickets exist and what is linked; do not move a half-linked ticket to
`ready`.

Finish with the list of filed tickets: id, title, URL, and what blocks each.

<ticket-template>

## Parent

A reference to the parent issue (only when the source was a tracker issue; otherwise omit this section).

## What to build

The end-to-end behaviour this ticket makes work, from the user's perspective, and why: not a layer-by-layer
implementation list.

## Acceptance criteria

- [ ] Criterion 1
- [ ] Criterion 2

## Out of scope

What a reader might expect here but belongs to another ticket (omit when nothing does).

</ticket-template>

The blocking edges are native links, so the body does not list them.

Avoid specific file paths or code snippets: they go stale fast. Exception: if a prototype produced a snippet that
encodes a decision more precisely than prose can (state machine, reducer, schema, type shape), inline it and note
briefly that it came from a prototype. Trim it to the decision-rich parts.
