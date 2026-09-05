#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
"$script_dir/assets/hydrate-assets.sh"
"$script_dir/zensical-docs.sh" build

# Fail the deployment if a dotfile or credential-pattern file slipped into the
# assembled site. check-docs.sh runs the full built-site validation; this gate
# guards every deployment.
python3 -c "import sys; sys.path.insert(0, '$script_dir/scripts'); \
import check_built_site; check_built_site.check_public_site_file_inventory()"
