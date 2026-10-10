#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
backup_dir="${1:-backups/$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$backup_dir"
backup_dir="$(cd "$backup_dir" && pwd)"
chmod 700 "$backup_dir"
docker compose stop app worker executor helix
trap 'docker compose start helix executor app worker' EXIT
project_name="${COMPOSE_PROJECT_NAME:-helix-foundry}"
for volume in helix-data foundry-data; do
  # The executor's spill files and the rows publication reads are temporary
  # and can take gigabytes.
  excludes=""
  if [ "$volume" = foundry-data ]; then excludes="--exclude=./scratch --exclude=./results"; fi
  docker run --rm -v "${project_name}_$volume:/data:ro" -v "$backup_dir:/backup" alpine:3.22 tar czf "/backup/$volume.tgz" $excludes -C /data .
done
cp .env "$backup_dir/config.env"
chmod 600 "$backup_dir/config.env"
echo "Backup saved to $backup_dir. Keep config.env private; it contains encryption keys."
