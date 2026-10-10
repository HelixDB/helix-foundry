#!/usr/bin/env bash
# scripts/restore.sh as released before backups left out the keys and before upgrade.sh.
# tests/scripts.test.ts checks out a release with it to test going back to it.
set -euo pipefail
cd "$(dirname "$0")/.."
backup_dir="$(cd "${1:?Usage: scripts/restore.sh BACKUP_DIR --replace}" && pwd)"
[ "${2:-}" = '--replace' ] || { echo 'Pass --replace to explicitly replace this installation with the backup.'; exit 1; }
for file in helix-data.tgz foundry-data.tgz config.env; do [ -f "$backup_dir/$file" ] || { echo "Missing $file"; exit 1; }; done
docker compose stop app worker executor helix
project_name="${COMPOSE_PROJECT_NAME:-helix-foundry}"
for volume in helix-data foundry-data; do
  docker run --rm -v "${project_name}_$volume:/data" -v "$backup_dir:/backup:ro" alpine:3.22 sh -c 'find /data -mindepth 1 -maxdepth 1 -exec rm -rf {} +; tar xzf "/backup/$1.tgz" -C /data' sh "$volume"
done
cp "$backup_dir/config.env" .env
chmod 600 .env
docker compose up -d helix executor app worker
