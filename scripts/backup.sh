#!/usr/bin/env bash
# Backs up the HelixDB and data volumes. The backup holds no ENCRYPTION_KEY or
# INTERNAL_KEY: keep a separate copy of .env. --include-key adds the whole .env,
# encrypted with a passphrase (BACKUP_PASSPHRASE, or typed in the terminal).
# Exit status 3 means the backup is complete, but the services did not start again.
set -euo pipefail
umask 077
here=$PWD
cd "$(dirname "$0")/.."
. scripts/backup-lib.sh
take_passphrase
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
[ -n "$backup_dir" ] || backup_dir=$(new_backup_dir)
backup_dir=$(from_here "$backup_dir")
outside='Choose a backup folder outside the repository, for example ~/helix-foundry-backups/NAME.'
[ -f .env ] || die 'There is no .env here. Run this from the checkout that runs Helix Foundry.'
fingerprint=$(key_fingerprint < .env)
[ -n "$fingerprint" ] || die 'ENCRYPTION_KEY in .env is missing or shorter than 32 characters.'
! inside_repo "$backup_dir" || die "$outside"
[ ! -d "$backup_dir" ] || [ -z "$(ls -A "$backup_dir")" ] \
  || die "$backup_dir is not empty. Use a new folder for each backup."
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
docker_ready
project_name="${COMPOSE_PROJECT_NAME:-helix-foundry}"
for volume in helix-data foundry-data; do
  docker volume inspect "${project_name}_$volume" > /dev/null 2>&1 \
    || die "Docker volume ${project_name}_$volume does not exist. Run this from the checkout that runs Helix Foundry, with the same COMPOSE_PROJECT_NAME."
done
mkdir -p "$backup_dir"
backup_dir="$(cd "$backup_dir" && pwd -P)"
if inside_repo "$backup_dir"; then
  rmdir "$backup_dir" 2> /dev/null || true
  die "$outside"
fi
[ -z "$(ls -A "$backup_dir")" ] || die "$backup_dir is not empty. Use a new folder for each backup."
chmod 700 "$backup_dir"
running_image=$(running_helix_image)

# Start again only the services that were running, so a stopped stack stays stopped.
running=" $(docker compose ps --services --status running 2> /dev/null | tr '\n' ' ') " \
  || running=" helix executor app worker "
restart=""
for service in helix executor app worker; do
  case $running in *" $service "*) restart="${restart:+$restart }$service" ;; esac
done
start_again() { [ -z "$restart" ] || docker compose start $restart; }
docker compose stop app worker executor helix
trap start_again EXIT
for volume in helix-data foundry-data; do
  docker run --rm -v "${project_name}_$volume:/data:ro" alpine:3.22 tar czf - -C /data . \
    > "$backup_dir/$volume.tgz.partial"
done
trap - EXIT
restarted=1
start_again || restarted=""

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
# The data comes from the commit the services were started from, which upgrade.sh
# and restore.sh record; without a record, it is the checked-out commit.
app_commit=$(recorded_commit || git rev-parse HEAD 2> /dev/null || echo unknown)
# Written last: a backup without manifest.txt is incomplete.
{
  echo '# Helix Foundry backup manifest. This file holds no keys; see scripts/restore.sh.'
  echo 'format=2'
  echo "created=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "app_commit=$app_commit"
  echo "app_schema=$(file_at "$app_commit" apps/api/src/store.ts | schema_version)"
  echo "helix_image=$(file_at "$app_commit" compose.yaml | helix_image)"
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
if [ -z "$restarted" ]; then
  echo "WARNING: the backup is complete, but docker compose start $restart failed, so those services are stopped. Fix the error above, then start them: docker compose up -d $restart" >&2
  exit 3
fi
