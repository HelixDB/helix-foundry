#!/usr/bin/env bash
# Replaces this installation's data with a backup from scripts/backup.sh. It keeps
# .env and refuses a backup made with another ENCRYPTION_KEY, unless --env-file
# names the .env kept for that backup or the backup holds one in env.enc. It checks
# everything before it stops anything, and replaces .env only once the data is in.
set -euo pipefail
umask 077
here=$PWD
cd "$(dirname "$0")/.."
. scripts/backup-lib.sh
take_passphrase
usage='Usage: scripts/restore.sh BACKUP_DIR --replace [--env-file FILE] [--accept-key-mismatch] [--accept-helix-mismatch]'
backup_dir="" replace="" env_file="" accept_mismatch="" accept_helix=""
while [ $# -gt 0 ]; do
  case $1 in
    --replace) replace=1 ;;
    --env-file) [ $# -ge 2 ] || die "$usage"; env_file=$2; shift ;;
    --accept-key-mismatch) accept_mismatch=1 ;;
    --accept-helix-mismatch) accept_helix=1 ;;
    -*) die "$usage" ;;
    *) [ -z "$backup_dir" ] || die "$usage"; backup_dir=$1 ;;
  esac
  shift
done
[ -n "$backup_dir" ] || die "$usage"
backup_dir=$(from_here "$backup_dir")
[ -d "$backup_dir" ] || die "No such folder: $backup_dir"
backup_dir="$(cd "$backup_dir" && pwd -P)"
[ -z "$env_file" ] || env_file=$(from_here "$env_file")
[ -n "$replace" ] || die 'Pass --replace to explicitly replace this installation with the backup.'
for file in helix-data.tgz foundry-data.tgz; do [ -s "$backup_dir/$file" ] || die "Missing $file"; done
manifest="$backup_dir/manifest.txt"
commit=$(manifest_field "$manifest" app_commit)
case $commit in
  "" | unknown) back="check out the Helix Foundry release that made the backup" ;;
  *) back="check out the commit that made the backup (git reset --keep $commit)" ;;
esac
unchanged='Nothing was changed.'

# Which ENCRYPTION_KEY the backup was made with. An older backup whose config.env
# was removed does not say, so it needs --env-file or --accept-key-mismatch.
if [ -f "$manifest" ] || [ -f "$backup_dir/config.env" ]; then
  if [ -f "$manifest" ]; then
    expected=$(manifest_field "$manifest" key_fingerprint)
  else
    expected=$(key_fingerprint < "$backup_dir/config.env")
  fi
  [ -n "$expected" ] || die "The backup does not record which ENCRYPTION_KEY it was made with. $unchanged"
else
  expected=""
  unverified="$backup_dir has no manifest.txt or config.env, so restore.sh cannot check which ENCRYPTION_KEY it was made with."
  [ -n "$env_file" ] || [ -n "$accept_mismatch" ] \
    || die "$unverified If an older backup.sh made it and its config.env was removed, pass --env-file with the copy of .env you kept for it, or --accept-key-mismatch to keep the current .env. $unchanged"
  echo "WARNING: $unverified If the key you use is another, re-enter the connector and AI credentials after the restore." >&2
fi
if [ -f "$backup_dir/config.env" ]; then
  echo 'WARNING: this backup has a config.env, which holds an encryption key in plain text.' >&2
  echo 'Anyone with a copy can decrypt the stored credentials. After restoring, make a new backup' >&2
  echo 'with scripts/backup.sh and delete the old copies.' >&2
fi

# HelixDB upgrades its storage one way, so newer data may not open in an older image.
# Docker can record the container's image without its tag; then the image the
# backup's commit pins gives the version, when it is the same image.
current_image=$(helix_image < compose.yaml)
pinned_image=$(manifest_field "$manifest" helix_image)
backup_image=$(manifest_field "$manifest" helix_image_running)
if [ -z "$backup_image" ] \
  || { [ -z "$(helix_version "$backup_image")" ] && [ -n "$pinned_image" ] && same_image "$backup_image" "$pinned_image"; }; then
  backup_image=$pinned_image
fi
backup_version=$(helix_version "$backup_image")
current_version=$(helix_version "$current_image")
made="HelixDB $backup_version made this backup"
newer="upgrade to a release that runs ${backup_version:+HelixDB }${backup_version:-$backup_image} or newer"
older="To restore it, $back, or $newer."
refuse() { # $1 = why
  [ -n "$accept_helix" ] || die "$1 HelixDB upgrades its storage one way, so an older HelixDB may refuse the data. $older If you know this HelixDB opens it, run again with --accept-helix-mismatch. $unchanged"
  echo "WARNING: $1 Restoring anyway, as asked. If HelixDB refuses the data (unsupported_index_storage_version in its logs), $back and restore again." >&2
}
if [ -z "$backup_image" ]; then
  echo 'WARNING: the backup does not record its HelixDB version. If HelixDB then refuses the data' >&2
  echo "(unsupported_index_storage_version in its logs), $back and restore again." >&2
elif same_image "$backup_image" "$current_image"; then
  :
elif [ -z "$backup_version" ] || [ -z "$current_version" ]; then
  refuse "$backup_image made this backup, and this checkout runs $current_image. restore.sh cannot tell which HelixDB version is newer."
elif version_newer "$backup_version" "$current_version"; then
  storage=$(helix_storage "$backup_version")
  if [ -n "$storage" ] && [ "$storage" = "$(helix_storage "$current_version")" ]; then
    echo "$made. This checkout runs HelixDB $current_version, which uses the same storage format."
  else
    refuse "$made, but this checkout runs HelixDB $current_version."
  fi
elif version_newer "$current_version" "$backup_version"; then
  if version_newer "$current_version" 0.0.9 && ! version_newer "$backup_version" 0.0.9; then
    echo "$made. This checkout runs HelixDB $current_version, which upgrades the restored data's index storage one way when it starts. The backup itself does not change."
  else
    echo "$made. This checkout runs HelixDB $current_version."
  fi
fi
backup_schema=$(manifest_field "$manifest" app_schema)
current_schema=$(schema_version < apps/api/src/store.ts)
if [ -n "$backup_schema" ] && [ -n "$current_schema" ] && [ "$backup_schema" -gt "$current_schema" ]; then
  die "The backup has metadata schema $backup_schema, newer than this checkout's $current_schema. To restore it, $back. $unchanged"
fi

# The .env to run with: --env-file, the current .env, env.enc, or an old config.env.
# A new one waits in $tmp until the data is restored.
mismatch() { echo "The ENCRYPTION_KEY in $1 is not the key this backup was made with, so the restored connector and AI credentials could not be decrypted."; }
tmp="" failed=""
cleanup() {
  local status=$?
  [ -z "$tmp" ] || rm -f "$tmp"
  [ "$status" = 0 ] || [ -z "$failed" ] || echo "$failed" >&2
}
trap cleanup EXIT
# Copies env file $1 into $tmp, reading it once, so it can come from a pipe, as with
# --env-file <(...), and checks the copy's key; $2 names it in messages.
candidate() {
  tmp=$(mktemp .env.XXXXXX)
  cat "$1" > "$tmp" 2> /dev/null || die "Could not read $2. $unchanged"
  from=$2
  if [ -n "$expected" ] && [ "$(key_fingerprint < "$tmp")" != "$expected" ]; then
    [ -n "$accept_mismatch" ] || die "$(mismatch "$2") $unchanged"
    echo "WARNING: $(mismatch "$2") Re-enter them after the restore." >&2
  fi
}
from=.env
current=""
[ ! -f .env ] || current=$(key_fingerprint < .env)
if [ -n "$env_file" ]; then
  [ -r "$env_file" ] && [ ! -d "$env_file" ] || die "Cannot read $env_file. $unchanged"
  candidate "$env_file" "$env_file"
elif [ -n "$expected" ] && [ "$current" = "$expected" ]; then
  echo 'Keeping .env: its ENCRYPTION_KEY matches the backup.'
elif [ -f .env ] && [ -n "$accept_mismatch" ]; then
  if [ -n "$expected" ]; then
    echo "WARNING: $(mismatch .env) Keeping .env as asked: re-enter them after the restore." >&2
  else
    echo 'Keeping .env, as asked.'
  fi
elif [ -n "$expected" ] && [ -f "$backup_dir/env.enc" ]; then
  echo 'Decrypting the .env stored in the backup (env.enc).'
  read_passphrase
  tmp=$(mktemp .env.XXXXXX)
  decrypt_env "$backup_dir/env.enc" "$(manifest_field "$manifest" key_iterations)" > "$tmp" 2> /dev/null \
    && [ "$(key_fingerprint < "$tmp")" = "$expected" ] \
    || die "Could not decrypt env.enc: check the passphrase. $unchanged"
  from="env.enc in the backup"
elif [ -f "$backup_dir/config.env" ]; then
  candidate "$backup_dir/config.env" "config.env in the backup"
elif [ ! -f .env ]; then
  die "There is no .env here, and the backup does not hold the keys. Pass --env-file with the copy of .env you kept for this installation. If that key is lost, run ./scripts/setup.sh to make a new .env, then run this again with --accept-key-mismatch and re-enter every stored credential afterwards. $unchanged"
else
  die "$(mismatch .env) Pass --env-file with the copy of .env you kept for this installation. If that key is lost, --accept-key-mismatch restores the data anyway and you re-enter every stored credential. $unchanged"
fi
[ -n "$(key_fingerprint < "${tmp:-.env}")" ] \
  || die "ENCRYPTION_KEY in $from is missing or shorter than 32 characters. $unchanged"
key_set INTERNAL_KEY < "${tmp:-.env}" || die "INTERNAL_KEY in $from is missing or too short. $unchanged"
if [ -n "$tmp" ] && [ -f .env ] && cmp -s .env "$tmp"; then
  rm -f "$tmp"
  tmp=""
fi

# A damaged copy would leave the installation half restored.
for file in helix-data.tgz foundry-data.tgz; do
  tar -tzf "$backup_dir/$file" > /dev/null 2>&1 \
    || die "$file in $backup_dir cannot be read, so the backup is damaged or incomplete. $unchanged"
done
docker_ready "$unchanged"
# Compose reads the keys from .env, so a new installation gets its .env first.
if [ ! -f .env ]; then
  mv "$tmp" .env
  tmp=""
  echo "Wrote .env from $from."
fi

failed='The restore did not finish: the services are stopped, and the data may be partly replaced. Fix the error, then run the same command again.'
docker compose stop app worker executor helix
project_name="${COMPOSE_PROJECT_NAME:-helix-foundry}"
for volume in helix-data foundry-data; do
  docker run --rm -i -v "${project_name}_$volume:/data" alpine:3.22 \
    sh -c 'find /data -mindepth 1 -maxdepth 1 -exec rm -rf {} + && tar xzf - -C /data' \
    < "$backup_dir/$volume.tgz"
done
if [ -n "$tmp" ]; then
  saved=".env.pre-restore-$(date +%Y%m%d-%H%M%S)"
  cp .env "$saved"
  chmod 600 "$saved"
  mv "$tmp" .env
  tmp=""
  echo "Saved the previous .env, with its keys, as $saved. Keep it private, and delete it once the restore is confirmed."
  echo "Wrote .env from $from."
fi
# A kept .env may have been readable by others.
chmod 600 .env
failed='The data is restored, but the services did not start. Fix the error, then run: docker compose up -d --build helix executor app worker'
# --build, so the restored data runs with this checkout's code.
docker compose up -d --build helix executor app worker
failed=""
record_deployed
echo "Restored $backup_dir."
