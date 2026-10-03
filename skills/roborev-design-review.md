---
title: "roborev-design-review"
description: "Review design proposals for completeness, feasibility, clarity, and technical soundness."
last_edited: 2026-03-16
---
# /roborev-design-review

Review a design proposal for completeness, feasibility, and technical soundness.

## Usage

```
/roborev-design-review <path-or-job-id>
```

## Description

Reviews design proposals (PRDs and task lists) for completeness, feasibility, and technical soundness. Unlike `/roborev-fix` which fixes code review findings, this skill evaluates design documents before implementation begins.

If a file path is given, that file is reviewed directly. If a job ID is given, the design output is fetched from roborev. If no argument is given, the skill looks for design docs in `docs/design/`.

## Instructions

When the user invokes `/roborev-design-review <path-or-job-id>`:

1. **Locate design documents**:
   - File path: read the file directly
   - Job ID: fetch with `roborev show --job <id>`
   - No argument: scan `docs/design/` for the most recent design doc

2. **Review the PRD** for:
   - Completeness (goals, non-goals, success criteria, edge cases)
   - Feasibility (technical decisions grounded in the codebase)
   - Clarity (decisions justified and understandable)
   - Missing considerations (security, performance, backwards compatibility)

3. **Review the task list** for:
   - Scoping (stages small enough to implement incrementally)
   - Ordering (dependencies correctly sequenced)
   - Coverage (tasks cover all PRD requirements)
   - Testability (verification steps included)

4. **Output structured findings** with severity ratings (high/medium/low) and a Pass/Fail verdict.

## Example

User: `/roborev-design-review docs/design/auth-redesign.md`

Agent:
1. Reads `docs/design/auth-redesign.md`
2. Reads relevant source files to verify assumptions
3. Reviews PRD completeness and feasibility
4. Reviews task list scoping and ordering
5. Outputs structured findings with severity ratings and verdict
