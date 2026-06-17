# /roborev-fix

Fix all open failing review findings in one pass.

## Usage

```
/roborev-fix [job_id...]
```

## Description

Discovers open failing code reviews and fixes all their findings in a single pass. This skill batches all actionable outstanding findings together, groups them by file, and fixes them by severity priority. It also handles single reviews when given a specific job ID.

If job IDs are provided, only those reviews are fixed. Otherwise, the skill checks recent commits (HEAD, HEAD~1) for failed reviews that have not been closed.

## Instructions

When the user invokes `/roborev-fix [job_id...]`:

1. **Discover reviews** to address:
   - If job IDs given, use those
   - Otherwise, run `roborev show HEAD` and `roborev show HEAD~1` to find open failing reviews
   - If no failed reviews found, inform the user

2. **Fetch all reviews** using `roborev show --job <id>` for each job.

3. **Parse and prioritize findings** from all reviews:
   - Collect severity, file paths, and line numbers
   - Group by file to minimize context switches
   - Order by severity (high first)

4. **Fix all findings** across all reviews.

5. **Run tests** to verify the fixes work.

6. **Record comments** for each fixed job:
   ```bash
   roborev comment --job <job_id> "<summary of changes>"
   ```

7. **Ask to commit** all changes together.

## Example

User: `/roborev-fix`

Agent:
1. Runs `roborev show HEAD` and `roborev show HEAD~1`
2. Finds 2 failed reviews: job 1019 (2 findings) and job 1021 (1 finding)
3. Fetches both reviews with `roborev show --job 1019` and `roborev show --job 1021`
4. Fixes all 3 findings across both reviews, prioritizing by severity
5. Runs tests to verify
6. Records comments on both jobs
7. Asks: "I've fixed 3 findings across 2 reviews. Tests pass. Would you like me to commit these changes?"
