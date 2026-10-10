#!/usr/bin/env bash
# Replaces this installation's data with a backup from scripts/backup.sh. It keeps
# .env and refuses a backup made with another ENCRYPTION_KEY, unless --env-file
# names the .env kept for that backup or the backup holds one in env.enc.
set -euo pipefail
umask 077
here=$PWD
cd "$(dirname "$0")/.."
. scripts/backup-lib.sh
usage='Usage: scripts/restore.sh BACKUP_DIR --replace [--env-file FILE] [--accept-key-mismatch]'
backup_dir="" replace="" env_file="" accept_mismatch=""
while [ $# -gt 0 ]; do
  case $1 in
    --replace) replace=1 ;;
    --env-file) [ $# -ge 2 ] || die "$usage"; env_file=$2; shift ;;
    --accept-key-mismatch) accept_mismatch=1 ;;
    -*) die "$usage" ;;
    *) [ -z "$backup_dir" ] || die "$usage"; backup_dir=$1 ;;
  esac
  shift
done
[ -n "$backup_dir" ] || die "$usage"
backup_dir="$(cd "$(from_here "$backup_dir")" && pwd -P)"
[ -z "$env_file" ] || env_file=$(from_here "$env_file")
[ -n "$replace" ] || die 'Pass --replace to explicitly replace this installation with the backup.'
for file in helix-data.tgz foundry-data.tgz; do [ -s "$backup_dir/$file" ] || die "Missing $file"; done
manifest="$backup_dir/manifest.txt"
commit=$(manifest_field "$manifest" app_commit)
case $commit in
  "" | unknown) back="check out the Helix Foundry release that made the backup" ;;
  *) back="check out the commit that made the backup (git checkout $commit)" ;;
esac
unchanged='Nothing was changed.'

# Which ENCRYPTION_KEY the backup was made with.
if [ -f "$manifest" ]; then
  expected=$(manifest_field "$manifest" key_fingerprint)
elif [ -f "$backup_dir/config.env" ]; then
  expected=$(key_fingerprint < "$backup_dir/config.env")
  echo 'WARNING: an older backup.sh made this backup, and its config.env holds your encryption key.' >&2
  echo 'Anyone with a copy can decrypt the stored credentials. After restoring, make a new backup' >&2
  echo 'with scripts/backup.sh and delete the old copies.' >&2
else
  die "$backup_dir is not a Helix Foundry backup: it has no manifest.txt or config.env."
fi
[ -n "$expected" ] || die "The backup does not record which ENCRYPTION_KEY it was made with. $unchanged"

# HelixDB upgrades its storage one way, so newer data cannot go into an older image.
current_image=$(helix_image < compose.yaml)
backup_image=$(manifest_field "$manifest" helix_image_running)
[ -n "$backup_image" ] || backup_image=$(manifest_field "$manifest" helix_image)
if [ -z "$backup_image" ]; then
  echo 'WARNING: the backup does not record its HelixDB version. If HelixDB then refuses the data' >&2
  echo "(unsupported_index_storage_version in its logs), $back and restore again." >&2
elif [ "$backup_image" != "$current_image" ]; then
  backup_version=$(helix_version "$backup_image")
  current_version=$(helix_version "$current_image")
  if [ -n "$backup_version" ] && [ -n "$current_version" ] && version_newer "$backup_version" "$current_version"; then
    die "HelixDB $backup_version made this backup, but this checkout runs HelixDB $current_version. HelixDB upgrades its storage one way, so an older HelixDB may refuse the data. To restore it, $back, or upgrade to a release with HelixDB $backup_version or newer. $unchanged"
  elif [ -n "$backup_version" ] && [ -n "$current_version" ]; then
    echo "HelixDB $backup_version made this backup. This checkout runs HelixDB $current_version, which upgrades the restored data one way when it starts. The backup itself does not change."
  else
    echo "WARNING: $backup_image made this backup, but this checkout runs $current_image." >&2
  fi
fi
backup_schema=$(manifest_field "$manifest" app_schema)
current_schema=$(schema_version)
if [ -n "$backup_schema" ] && [ -n "$current_schema" ] && [ "$backup_schema" -gt "$current_schema" ]; then
  die "The backup has metadata schema $backup_schema, newer than this checkout's $current_schema. To restore it, $back. $unchanged"
fi

# The .env to run with: --env-file, the current .env, env.enc, or an old config.env.
mismatch() { echo "The ENCRYPTION_KEY in $1 is not the key this backup was made with, so the restored connector and AI credentials could not be decrypted."; }
tmp=""
trap '[ -z "$tmp" ] || rm -f "$tmp"' EXIT
new_env="" from=.env
current=""
[ ! -f .env ] || current=$(key_fingerprint < .env)
if [ -n "$env_file" ]; then
  [ -f "$env_file" ] || die "No such file: $env_file"
  if [ "$(key_fingerprint < "$env_file")" != "$expected" ]; then
    [ -n "$accept_mismatch" ] || die "$(mismatch "$env_file") $unchanged"
    echo "WARNING: $(mismatch "$env_file") Re-enter them after the restore." >&2
  fi
  new_env=$env_file from=$env_file
elif [ -n "$current" ] && [ "$current" = "$expected" ]; then
  echo 'Keeping .env: its ENCRYPTION_KEY matches the backup.'
elif [ -f "$backup_dir/env.enc" ]; then
  echo 'Decrypting the .env stored in the backup (env.enc).'
  read_passphrase
  tmp=$(mktemp .env.XXXXXX)
  decrypt_env "$backup_dir/env.enc" "$(manifest_field "$manifest" key_iterations)" > "$tmp" 2> /dev/null \
    && [ "$(key_fingerprint < "$tmp")" = "$expected" ] \
    || die "Could not decrypt env.enc: check the passphrase. $unchanged"
  new_env=$tmp from="env.enc in the backup"
elif [ -f "$backup_dir/config.env" ]; then
  new_env=$backup_dir/config.env from="config.env in the backup"
elif [ ! -f .env ]; then
  die "There is no .env here, and the backup does not hold the keys. Pass --env-file with the copy of .env you kept for this installation. $unchanged"
elif [ -n "$accept_mismatch" ]; then
  echo "WARNING: $(mismatch .env) Re-enter them after the restore." >&2
else
  die "$(mismatch .env) Pass --env-file with the copy of .env you kept for this installation. If that key is lost, --accept-key-mismatch restores the data anyway and you re-enter every stored credential. $unchanged"
fi
grep -Eq '^INTERNAL_KEY=.{32,}' "${new_env:-.env}" || die "INTERNAL_KEY in $from is missing or too short. $unchanged"

if [ -n "$new_env" ] && ! { [ -f .env ] && cmp -s .env "$new_env"; }; then
  if [ -f .env ]; then
    saved=".env.pre-restore-$(date +%Y%m%d-%H%M%S)"
    cp -p .env "$saved"
    echo "Saved the previous .env as $saved."
  fi
  if [ "$new_env" != "$tmp" ]; then
    tmp=$(mktemp .env.XXXXXX)
    cat "$new_env" > "$tmp"
  fi
  chmod 600 "$tmp"
  mv "$tmp" .env
  tmp=""
  echo "Wrote .env from $from."
fi
docker compose stop app worker executor helix
project_name="${COMPOSE_PROJECT_NAME:-helix-foundry}"
for volume in helix-data foundry-data; do
  docker run --rm -i -v "${project_name}_$volume:/data" alpine:3.22 \
    sh -c 'find /data -mindepth 1 -maxdepth 1 -exec rm -rf {} + && tar xzf - -C /data' \
    < "$backup_dir/$volume.tgz"
done
# --build, so the restored data runs with this checkout's code.
docker compose up -d --build helix executor app worker
echo "Restored $backup_dir."
