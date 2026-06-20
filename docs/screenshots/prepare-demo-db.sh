#!/usr/bin/env bash
# Prepare an isolated demo database from real roborev reviews of public repos.

set -euo pipefail

SOURCE_DB="${ROBOREV_DOCS_SOURCE_DB:-${ROBOREV_DATA_DIR:-$HOME/.roborev}/reviews.db}"
DEMO_DIR="${TMPDIR:-/tmp}/roborev-demo-data"
DEST_DB="$DEMO_DIR/reviews.db"

if [[ ! -f "$SOURCE_DB" ]]; then
  echo "Error: source database not found at $SOURCE_DB" >&2
  echo "Set ROBOREV_DOCS_SOURCE_DB to a roborev reviews.db with public project reviews." >&2
  exit 1
fi

if ! command -v python3 >/dev/null 2>&1; then
  echo "Error: python3 is required to sanitize and copy screenshot data" >&2
  exit 1
fi

mkdir -p "$DEMO_DIR"
rm -f "$DEST_DB" "$DEST_DB-wal" "$DEST_DB-shm"

echo "Source: $SOURCE_DB"
echo "Destination: $DEST_DB"
echo ""

export SOURCE_DB DEST_DB
python3 <<'PY'
import os
import pathlib
import re
import sqlite3
import sys

allowed_repos = ("roborev", "kata", "msgvault", "agentsview")
terminal_statuses = ("done", "failed", "canceled", "applied", "rebased", "skipped")
limit = int(os.environ.get("ROBOREV_DOCS_REVIEW_LIMIT", "1000"))
source_db = os.environ["SOURCE_DB"]
dest_db = os.environ["DEST_DB"]
home = str(pathlib.Path.home())
home_name = pathlib.Path.home().name

secret_patterns = [
    re.compile(r"sk-[A-Za-z0-9_-]{12,}"),
    re.compile(r"gh[pousr]_[A-Za-z0-9_]{12,}"),
    re.compile(r"AKIA[0-9A-Z]{16}"),
    re.compile(
        r"(?i)\b(api[_-]?key|secret|password)\b([\"']?\s*[:=]\s*[\"']?)[^\s,\"')]+"
    ),
]


def connect_readonly(path):
    uri = "file:" + pathlib.Path(path).resolve().as_posix() + "?mode=ro"
    return sqlite3.connect(uri, uri=True)


src = connect_readonly(source_db)
src.row_factory = sqlite3.Row
dst = sqlite3.connect(dest_db)
dst.row_factory = sqlite3.Row


def exec_schema():
    rows = src.execute(
        """
        SELECT type, name, sql
        FROM sqlite_master
        WHERE sql IS NOT NULL
          AND name NOT LIKE 'sqlite_%'
          AND type IN ('table', 'index')
        ORDER BY CASE type WHEN 'table' THEN 0 ELSE 1 END, name
        """
    ).fetchall()
    for row in rows:
        try:
            dst.execute(row["sql"])
        except sqlite3.OperationalError as exc:
            raise SystemExit(f"create {row['type']} {row['name']}: {exc}") from exc


def table_columns(conn, table):
    return [row["name"] for row in conn.execute(f"PRAGMA table_info({ident(table)})")]


def has_table(conn, table):
    return (
        conn.execute(
            "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?", (table,)
        ).fetchone()
        is not None
    )


def qmarks(n):
    return ",".join("?" for _ in range(n))


def ident(name):
    return '"' + str(name).replace('"', '""') + '"'


exec_schema()


all_repo_rows = src.execute(
    f"SELECT * FROM repos WHERE name IN ({qmarks(len(allowed_repos))})",
    allowed_repos,
).fetchall()
if not all_repo_rows:
    raise SystemExit(
        "no public docs repos found in source database: " + ", ".join(allowed_repos)
    )

repo_job_counts = {
    row["repo_id"]: row["count"]
    for row in src.execute(
        f"""
        SELECT repo_id, COUNT(*) AS count
        FROM review_jobs
        WHERE status IN ({qmarks(len(terminal_statuses))})
        GROUP BY repo_id
        """,
        terminal_statuses,
    )
}
repo_rows = []
for name in allowed_repos:
    matches = [row for row in all_repo_rows if row["name"] == name]
    if matches:
        repo_rows.append(
            max(matches, key=lambda row: (repo_job_counts.get(row["id"], 0), row["id"]))
        )

repo_by_id = {row["id"]: row for row in repo_rows}
repo_ids = tuple(repo_by_id)
repo_roots = {row["root_path"]: f"/repos/{row['name']}" for row in repo_rows}


def sanitize_text(value):
    if value is None or not isinstance(value, str):
        return value

    sanitized = value
    for original, replacement in sorted(repo_roots.items(), key=lambda item: -len(item[0])):
        if original:
            sanitized = sanitized.replace(original, replacement)
    if home:
        sanitized = sanitized.replace(home, "/home/maintainer")
    if home_name:
        sanitized = re.sub(r"\b" + re.escape(home_name) + r"\b", "maintainer", sanitized)
    sanitized = sanitized.replace("/Users/", "/home/maintainer/")
    sanitized = sanitized.replace(r"C:\Users\\", r"C:\Users\maintainer\\")

    sanitized = re.sub(
        r"/Users/[A-Za-z0-9._-]+(?:/[^\s\"'`)>\]]*)?",
        "/home/maintainer",
        sanitized,
    )
    sanitized = re.sub(
        r"/home/(?!maintainer\b)[A-Za-z0-9._-]+(?:/[^\s\"'`)>\]]*)?",
        "/home/maintainer",
        sanitized,
    )
    sanitized = re.sub(
        r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}",
        "maintainer@example.com",
        sanitized,
    )
    for pattern in secret_patterns:
        sanitized = pattern.sub(lambda m: m.group(1) + m.group(2) + "[REDACTED]" if m.lastindex == 2 else "[REDACTED]", sanitized)
    return sanitized


def insert_row(table, row, overrides=None):
    overrides = overrides or {}
    cols = table_columns(dst, table)
    row_keys = set(row.keys())
    values = []
    for col in cols:
        if col in overrides:
            value = overrides[col]
        elif col in row_keys:
            value = row[col]
        else:
            value = None
        values.append(sanitize_text(value))
    dst.execute(
        f"INSERT INTO {ident(table)} ({', '.join(ident(col) for col in cols)}) VALUES ({qmarks(len(cols))})",
        values,
    )


def review_is_failing(row):
    verdict = row["review_verdict_bool"]
    if verdict is not None:
        return int(verdict) == 0
    output = row["review_output"] or ""
    if re.search(r"\bP/F:\s*F\b", output, re.IGNORECASE):
        return True
    if re.search(r"^\s*(?:[-*]\s*)?(?:Critical|High|Medium|Low)\s*[:\-\u2013\u2014]", output, re.IGNORECASE | re.MULTILINE):
        return True
    return row["status"] == "failed"


status_clause = qmarks(len(terminal_statuses))
candidate_rows = src.execute(
    f"""
    SELECT
      j.*,
      rv.output AS review_output,
      rv.verdict_bool AS review_verdict_bool
    FROM review_jobs j
    LEFT JOIN reviews rv ON rv.job_id = j.id
    WHERE j.repo_id IN ({qmarks(len(repo_ids))})
      AND j.status IN ({status_clause})
    ORDER BY datetime(COALESCE(j.finished_at, j.started_at, j.enqueued_at)) DESC, j.id DESC
    """,
    repo_ids + terminal_statuses,
).fetchall()
if not candidate_rows:
    raise SystemExit("no terminal review jobs found for public docs repos")

failing = [row for row in candidate_rows if review_is_failing(row)]
passing = [row for row in candidate_rows if not review_is_failing(row)]
target_failing = min(len(failing), int(limit * 0.9))
target_passing = min(len(passing), limit - target_failing)

selected_ids = {row["id"] for row in failing[:target_failing]}
selected_ids.update(row["id"] for row in passing[:target_passing])
if len(selected_ids) < min(limit, len(candidate_rows)):
    for row in candidate_rows:
        if row["id"] not in selected_ids:
            selected_ids.add(row["id"])
        if len(selected_ids) >= min(limit, len(candidate_rows)):
            break

selected_jobs = [row for row in candidate_rows if row["id"] in selected_ids]
job_id_map = {row["id"]: len(selected_jobs) - idx for idx, row in enumerate(selected_jobs)}
selected_job_ids = tuple(job_id_map)
commit_ids = tuple(
    sorted({row["commit_id"] for row in selected_jobs if row["commit_id"] is not None})
)

dst.execute("PRAGMA foreign_keys = OFF")
dst.execute("BEGIN")

for row in repo_rows:
    insert_row(
        "repos",
        row,
        {
            "root_path": f"/repos/{row['name']}",
            "identity": f"github.com/kenn-io/{row['name']}",
        },
    )

if commit_ids:
    for row in src.execute(
        f"SELECT * FROM commits WHERE id IN ({qmarks(len(commit_ids))})", commit_ids
    ):
        insert_row("commits", row)

for row in selected_jobs:
    insert_row(
        "review_jobs",
        row,
        {
            "id": job_id_map[row["id"]],
            "source_machine_id": None,
            "synced_at": None,
            "worker_id": sanitize_text(row["worker_id"]),
            "worktree_path": "",
        },
    )

for row in src.execute(
    f"SELECT * FROM reviews WHERE job_id IN ({qmarks(len(selected_job_ids))})",
    selected_job_ids,
):
    insert_row(
        "reviews",
        row,
        {
            "job_id": job_id_map[row["job_id"]],
            "updated_by_machine_id": None,
            "synced_at": None,
        },
    )

if has_table(src, "responses"):
    response_cols = table_columns(src, "responses")
    predicates = []
    params = []
    if commit_ids and "commit_id" in response_cols:
        predicates.append(f"commit_id IN ({qmarks(len(commit_ids))})")
        params.extend(commit_ids)
    if selected_job_ids and "job_id" in response_cols:
        predicates.append(f"job_id IN ({qmarks(len(selected_job_ids))})")
        params.extend(selected_job_ids)
    if predicates:
        for row in src.execute(
            "SELECT * FROM responses WHERE " + " OR ".join(predicates), params
        ):
            overrides = {"source_machine_id": None, "synced_at": None}
            if "job_id" in response_cols and row["job_id"] in job_id_map:
                overrides["job_id"] = job_id_map[row["job_id"]]
            insert_row("responses", row, overrides)

dst.execute("COMMIT")
dst.execute("PRAGMA foreign_keys = ON")


def validate_sanitized():
    failures = []
    private_patterns = [
        re.compile(r"/Users/"),
        re.compile(re.escape(home)) if home else None,
        re.compile(r"\b" + re.escape(home_name) + r"\b") if home_name else None,
        re.compile(r"sk-[A-Za-z0-9_-]{12,}"),
        re.compile(r"gh[pousr]_[A-Za-z0-9_]{12,}"),
        re.compile(r"AKIA[0-9A-Z]{16}"),
        re.compile(r"(?i)\b(api[_-]?key|secret|password)\b\s*[:=]\s*(?!\[REDACTED\])[^\s,\"')]+"),
    ]
    private_patterns = [p for p in private_patterns if p is not None]
    tables = dst.execute(
        "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'"
    ).fetchall()
    for table_row in tables:
        table = table_row["name"]
        text_cols = [
            col["name"]
            for col in dst.execute(f"PRAGMA table_info({ident(table)})")
            if "TEXT" in (col["type"] or "").upper()
        ]
        for col in text_cols:
            row_number = 0
            for row in dst.execute(
                f"SELECT {ident(col)} FROM {ident(table)} WHERE {ident(col)} IS NOT NULL"
            ):
                row_number += 1
                text = str(row[col])
                if any(pattern.search(text) for pattern in private_patterns):
                    failures.append(f"{table}.{col} row {row_number}")
                    if len(failures) >= 20:
                        break
            if len(failures) >= 20:
                break
        if len(failures) >= 20:
            break
    if failures:
        raise SystemExit(
            "sanitized screenshot database still contains private markers:\n"
            + "\n".join(failures)
        )


validate_sanitized()
dst.commit()

copied_failures = sum(1 for row in selected_jobs if review_is_failing(row))
copied_passes = len(selected_jobs) - copied_failures
missing_repos = [name for name in allowed_repos if name not in {row["name"] for row in repo_rows}]
if missing_repos:
    print("Warning: source database did not contain repos: " + ", ".join(missing_repos), file=sys.stderr)

print("Demo database created successfully")
print(f"Repos: {len(repo_rows)}")
print(f"Commits: {len(commit_ids)}")
print(f"Review Jobs: {len(selected_jobs)}")
print(f"Failing Reviews: {copied_failures}")
print(f"Passing Reviews: {copied_passes}")
print(f"Source Repos: {', '.join(row['name'] for row in repo_rows)}")
PY

echo ""
echo "To use: ROBOREV_DATA_DIR=$DEMO_DIR roborev tui"
