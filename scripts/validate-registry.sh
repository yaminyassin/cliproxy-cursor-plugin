#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)

python3 - "$REPO_DIR/registry.json" <<'PY'
import json
import pathlib
import re
import sys

registry_path = pathlib.Path(sys.argv[1])
registry = json.loads(registry_path.read_text(encoding="utf-8"))

if registry.get("schema_version") != 1:
    raise SystemExit("registry.json: schema_version must be 1")

plugins = registry.get("plugins")
if not isinstance(plugins, list) or not plugins:
    raise SystemExit("registry.json: plugins must be a non-empty list")

required = ("id", "name", "description", "author", "repository")
plugin_id_pattern = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
repository_pattern = re.compile(r"^https://github\.com/[^/]+/[^/]+$")
seen_ids: set[str] = set()

for index, plugin in enumerate(plugins):
    if not isinstance(plugin, dict):
        raise SystemExit(f"registry.json: plugins[{index}] must be an object")
    for field in required:
        if not isinstance(plugin.get(field), str) or not plugin[field].strip():
            raise SystemExit(f"registry.json: plugins[{index}].{field} is required")
    plugin_id = plugin["id"]
    if not plugin_id_pattern.fullmatch(plugin_id):
        raise SystemExit(f"registry.json: invalid plugin id {plugin_id!r}")
    if plugin_id in seen_ids:
        raise SystemExit(f"registry.json: duplicate plugin id {plugin_id!r}")
    seen_ids.add(plugin_id)
    if not repository_pattern.fullmatch(plugin["repository"]):
        raise SystemExit(f"registry.json: invalid GitHub repository for {plugin_id!r}")
    version = plugin.get("version", "")
    if version and (not isinstance(version, str) or version.startswith(("v", "V"))):
        raise SystemExit(f"registry.json: version for {plugin_id!r} must not start with v")

print(f"Validated {len(plugins)} maintained plugin entries")
PY
