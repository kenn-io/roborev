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
import subprocess
import sys

allowed_repos = ("roborev", "kata", "msgvault", "agentsview")
canonical_repos = {name: f"github.com/kenn-io/{name}" for name in allowed_repos}
review_statuses = ("done",)
limit = int(os.environ.get("ROBOREV_DOCS_REVIEW_LIMIT", "1000"))
source_db = os.environ["SOURCE_DB"]
dest_db = os.environ["DEST_DB"]
home = str(pathlib.Path.home())
home_name = pathlib.Path.home().name

windows_user_path_re = re.compile(
    r"(?i)\b[A-Z]:[\\/]+Users(?:[\\/]+[A-Za-z0-9._-]+(?:[\\/][^\s\"'`)>\]]*)?)?"
)
secret_patterns = [
    re.compile(r"sk-[A-Za-z0-9_-]{12,}"),
    re.compile(r"gh[pousr]_[A-Za-z0-9_]{12,}"),
    re.compile(r"github_pat_[A-Za-z0-9_]{12,}"),
    re.compile(r"glpat-[A-Za-z0-9_-]{12,}"),
    re.compile(r"AKIA[0-9A-Z]{16}"),
    re.compile(
        r"(?i)\b(api[_-]?key|secret|password|token)\b([\"']?\s*[:=]\s*[\"']?)[^\s,\"')]+"
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


def normalize_github_repo(value):
    if not value:
        return ""
    text = str(value).strip()
    patterns = (
        r"^git@github\.com:([^/\s]+)/([^/\s]+?)(?:\.git)?/?$",
        r"^ssh://git@github\.com/([^/\s]+)/([^/\s]+?)(?:\.git)?/?$",
        r"^https?://github\.com/([^/\s]+)/([^/\s]+?)(?:\.git)?/?$",
        r"^github\.com[:/]([^/\s]+)/([^/\s]+?)(?:\.git)?/?$",
    )
    for pattern in patterns:
        match = re.match(pattern, text, re.IGNORECASE)
        if match:
            owner, repo = match.groups()
            return f"github.com/{owner.lower()}/{repo.lower()}"
    return ""


def origin_remote_urls(root_path):
    if not root_path:
        return []
    path = pathlib.Path(root_path)
    if not path.exists():
        return []
    try:
        result = subprocess.run(
            ["git", "-C", str(path), "remote", "get-url", "--all", "origin"],
            check=False,
            capture_output=True,
            text=True,
            timeout=5,
        )
    except (OSError, subprocess.TimeoutExpired):
        return []
    if result.returncode != 0:
        return []
    return [line.strip() for line in result.stdout.splitlines() if line.strip()]


def repo_matches_canonical(row, expected):
    candidates = [row["identity"], row["root_path"]]
    candidates.extend(origin_remote_urls(row["root_path"]))
    return any(normalize_github_repo(candidate) == expected for candidate in candidates)


all_repo_rows = src.execute("SELECT * FROM repos").fetchall()
if not all_repo_rows:
    raise SystemExit("source database has no repos")
repo_rows = []
for name in allowed_repos:
    expected = canonical_repos[name]
    matches = [
        row
        for row in all_repo_rows
        if row["name"] == name and repo_matches_canonical(row, expected)
    ]
    if len(matches) != 1:
        detail = "missing" if len(matches) == 0 else f"ambiguous ({len(matches)} matches)"
        raise SystemExit(f"public docs repo {name!r} is {detail}; expected canonical {expected}")
    repo_rows.append(matches[0])

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
    sanitized = windows_user_path_re.sub("/home/maintainer", sanitized)

    sanitized = re.sub(
        r"/Users/[A-Za-z0-9._-]+(?:/[^\s\"'`)>\]]*)?",
        "/home/maintainer",
        sanitized,
    )
    sanitized = sanitized.replace("/Users/", "/home/maintainer/")
    sanitized = re.sub(
        r"/home/(?!maintainer\b)[A-Za-z0-9._-]+(?:/[^\s\"'`)>\]]*)?",
        "/home/maintainer",
        sanitized,
    )
    sanitized = re.sub(
        r"/home/maintainer/[A-Za-z0-9._-]+(?:/[^\s\"'`)>\]]*)?",
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


def row_value(row, key, default=""):
    if key not in row.keys():
        return default
    value = row[key]
    if value is None or value == "":
        return default
    return value


def review_is_failing(row):
    verdict = row["review_verdict_bool"]
    if verdict is not None:
        return int(verdict) == 0
    output = row["review_output"] or ""
    if re.search(r"\bP/F:\s*F\b", output, re.IGNORECASE):
        return True
    if re.search(r"^\s*(?:[-*]\s*)?(?:Critical|High|Medium|Low)\s*[:\-\u2013\u2014]", output, re.IGNORECASE | re.MULTILINE):
        return True
    return False


status_clause = qmarks(len(review_statuses))
candidate_rows = src.execute(
    f"""
    SELECT
      j.*,
      c.subject AS commit_subject,
      rv.output AS review_output,
      rv.verdict_bool AS review_verdict_bool
    FROM review_jobs j
    JOIN commits c ON c.id = j.commit_id
    JOIN reviews rv ON rv.job_id = j.id
    WHERE j.repo_id IN ({qmarks(len(repo_ids))})
      AND j.status IN ({status_clause})
      AND j.commit_id IS NOT NULL
      AND COALESCE(rv.output, '') <> ''
      AND COALESCE(NULLIF(j.job_type, ''), 'review') = 'review'
      AND COALESCE(j.dirty_files, '') IN ('', '[]', 'null')
    ORDER BY datetime(COALESCE(j.finished_at, j.started_at, j.enqueued_at)) DESC, j.id DESC
    """,
    repo_ids + review_statuses,
).fetchall()
if not candidate_rows:
    raise SystemExit("no completed reviewed jobs found for public docs repos")

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
selected_jobs_by_id = {row["id"]: row for row in selected_jobs}
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
            "dirty_files": "[]",
            "error": None,
            "source_machine_id": None,
            "synced_at": None,
            "worker_id": "docs-demo",
            "output_prefix": "",
            "patch": None,
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

dst.execute("COMMIT")
dst.execute("PRAGMA foreign_keys = ON")


def validate_sanitized():
    failures = []
    private_patterns = [
        re.compile(r"/Users/"),
        windows_user_path_re,
        re.compile(re.escape(home)) if home else None,
        re.compile(r"\b" + re.escape(home_name) + r"\b") if home_name else None,
        re.compile(r"/home/maintainer/[A-Za-z0-9._-]+"),
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
