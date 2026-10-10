#!/usr/bin/env bash
# Backs up the HelixDB and data volumes. The backup holds no ENCRYPTION_KEY or
# INTERNAL_KEY: keep a separate copy of .env. --include-key adds the whole .env,
# encrypted with a passphrase (BACKUP_PASSPHRASE, or typed in the terminal).
set -euo pipefail
umask 077
here=$PWD
cd "$(dirname "$0")/.."
. scripts/backup-lib.sh
usage='Usage: scripts/backup.sh [--include-key] [BACKUP_DIR]'
include_key="" backup_dir=""
while [ $# -gt 0 ]; do
  case $1 in
    --include-key) include_key=1 ;;
    -*) die "$usage" ;;
    *) [ -z "$backup_dir" ] || die "$usage"; backup_dir=$1 ;;
  esac
  shift
done
backup_dir=$(from_here "${backup_dir:-$HOME/helix-foundry-backups/$(date +%Y%m%d-%H%M%S)}")
[ -f .env ] || die 'There is no .env here. Run this from the checkout that runs Helix Foundry.'
fingerprint=$(key_fingerprint < .env)
[ -n "$fingerprint" ] || die 'ENCRYPTION_KEY in .env is missing or shorter than 32 characters.'
if [ -n "$include_key" ]; then
  cat >&2 <<'NOTE'
WARNING: --include-key stores your whole .env in the backup as env.enc, encrypted
with a passphrase. It holds ENCRYPTION_KEY, which decrypts every stored connector
and AI credential. Anyone with the backup and the passphrase can read them all.
Use a long passphrase and never keep it with the backup.
NOTE
  read_passphrase new
  [ "${#passphrase}" -ge 12 ] || die 'Use a passphrase of at least 12 characters.'
fi
mkdir -p "$backup_dir"
backup_dir="$(cd "$backup_dir" && pwd -P)"
if inside_repo "$backup_dir"; then
  rmdir "$backup_dir" 2> /dev/null || true
  die 'Choose a backup folder outside the repository, for example ~/helix-foundry-backups/NAME.'
fi
[ -z "$(ls -A "$backup_dir")" ] || die "$backup_dir is not empty. Use a new folder for each backup."
chmod 700 "$backup_dir"
project_name="${COMPOSE_PROJECT_NAME:-helix-foundry}"
for volume in helix-data foundry-data; do
  if ! docker volume inspect "${project_name}_$volume" > /dev/null 2>&1; then
    rmdir "$backup_dir"
    die "Docker volume ${project_name}_$volume does not exist. Run this from the checkout that runs Helix Foundry, with the same COMPOSE_PROJECT_NAME."
  fi
done
helix_container=$(docker compose ps -a -q helix 2> /dev/null | head -n 1 || true)
running_image=""
if [ -n "$helix_container" ]; then
  running_image=$(docker inspect --format '{{.Config.Image}}' "$helix_container" 2> /dev/null || true)
fi
docker compose stop app worker executor helix
trap 'docker compose start helix executor app worker' EXIT
for volume in helix-data foundry-data; do
  docker run --rm -v "${project_name}_$volume:/data:ro" alpine:3.22 tar czf - -C /data . \
    > "$backup_dir/$volume.tgz.partial"
done
trap - EXIT
docker compose start helix executor app worker
for volume in helix-data foundry-data; do
  tar -tzf "$backup_dir/$volume.tgz.partial" > /dev/null \
    || die "$volume.tgz.partial in $backup_dir cannot be read back, so this backup is incomplete."
  mv "$backup_dir/$volume.tgz.partial" "$backup_dir/$volume.tgz"
done
grep -E "$settings_pattern" .env > "$backup_dir/settings.env" || true
key_file=none
if [ -n "$include_key" ]; then
  encrypt_env .env "$backup_dir/env.enc"
  [ "$(decrypt_env "$backup_dir/env.enc" | key_fingerprint)" = "$fingerprint" ] \
    || die "env.enc in $backup_dir does not decrypt to this .env, so this backup is incomplete."
  key_file=env.enc
fi
# Written last: a backup without manifest.txt is incomplete.
{
  echo '# Helix Foundry backup manifest. This file holds no keys; see scripts/restore.sh.'
  echo 'format=2'
  echo "created=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "app_commit=$(git rev-parse HEAD 2> /dev/null || echo unknown)"
  echo "app_schema=$(schema_version)"
  echo "helix_image=$(helix_image < compose.yaml)"
  echo "helix_image_running=$running_image"
  echo "compose_project=$project_name"
  echo "key_fingerprint=$fingerprint"
  echo "key_file=$key_file"
  [ "$key_file" = none ] || echo "key_iterations=$key_iterations"
} > "$backup_dir/manifest.txt"
echo "Backup saved to $backup_dir."
if [ -n "$include_key" ]; then
  echo 'It includes .env in env.enc, encrypted with your passphrase. Keep the backup private.'
else
  echo 'It does not include ENCRYPTION_KEY. Keep a separate copy of .env, for example in a'
  echo 'password manager: without it, restored connector and AI credentials cannot be decrypted.'
fi
