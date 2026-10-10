#!/usr/bin/env bash
# Upgrades this installation. It takes and checks a fresh backup first, then
# fast-forwards the checkout to its upstream branch, rebuilds and restarts. If the
# HelixDB image changes, which can be one way, it asks first (or takes --yes).
set -euo pipefail
cd "$(dirname "$0")/.."
. scripts/backup-lib.sh
usage='Usage: scripts/upgrade.sh [--backup-dir DIR] [--include-key] [--yes]'
backup_dir="" include_key="" yes=""
while [ $# -gt 0 ]; do
  case $1 in
    --backup-dir) [ $# -ge 2 ] || die "$usage"; backup_dir=$2; shift ;;
    --include-key) include_key=--include-key ;;
    --yes) yes=1 ;;
    *) die "$usage" ;;
  esac
  shift
done
backup_dir=${backup_dir:-$HOME/helix-foundry-backups/$(date +%Y%m%d-%H%M%S)}
unchanged='Nothing was changed.'
[ -f .env ] || die 'There is no .env here. upgrade.sh upgrades an existing installation; see AGENTS.md to install.'
fingerprint=$(key_fingerprint < .env)
[ -n "$fingerprint" ] || die 'ENCRYPTION_KEY in .env is missing or shorter than 32 characters.'
upstream=$(git rev-parse --abbrev-ref --symbolic-full-name '@{u}' 2> /dev/null) \
  || die 'This branch has no upstream to upgrade from. Switch to the release branch, for example: git switch main'
[ -z "$(git status --porcelain --untracked-files=no)" ] \
  || die "This checkout has uncommitted changes. Commit or stash them first. $unchanged"
old_commit=$(git rev-parse HEAD)
old_image=$(helix_image < compose.yaml)
echo "Fetching $upstream."
git fetch --quiet || die "Could not fetch $upstream. $unchanged"
new_commit=$(git rev-parse '@{u}')
if [ "$new_commit" = "$old_commit" ]; then
  echo "Already up to date at $old_commit."
  exit 0
fi
git merge-base --is-ancestor HEAD "$new_commit" \
  || die "This checkout has commits that are not on $upstream, so it cannot fast-forward. $unchanged"
new_image=$(git show "$new_commit:compose.yaml" | helix_image)
old_version=$(helix_version "$old_image")
new_version=$(helix_version "$new_image")
if [ -n "$old_version" ] && [ -n "$new_version" ] && version_newer "$old_version" "$new_version"; then
  die "$new_commit moves HelixDB back from $old_version to $new_version, and an older HelixDB may refuse data a newer one opened. Upgrade by hand, after a backup. $unchanged"
fi
if [ "$new_image" != "$old_image" ]; then
  cat >&2 <<WARNING
WARNING: this upgrade changes the HelixDB image
  from $old_image
  to   $new_image
HelixDB upgrades its storage one way the first time a newer version opens it: from
v0.0.10 on it upgrades the index storage, and v0.0.9 and earlier then refuse the
database. Once it starts, going back to $old_commit needs the backup
this script takes first: git checkout $old_commit, then scripts/restore.sh BACKUP_DIR --replace.
WARNING
  if [ -z "$yes" ]; then
    [ -t 0 ] || die "Run again with --yes to accept the HelixDB upgrade. $unchanged"
    printf 'Type yes to upgrade HelixDB: ' >&2
    read -r answer
    [ "$answer" = yes ] || die "Upgrade cancelled. $unchanged"
  fi
fi

echo "Backing up to $backup_dir before upgrading."
"$BASH" scripts/backup.sh $include_key "$backup_dir" \
  || die 'Upgrade stopped: the backup failed. Nothing was upgraded.'
backup_dir="$(cd "$backup_dir" && pwd -P)"
for file in helix-data.tgz foundry-data.tgz manifest.txt; do
  [ -s "$backup_dir/$file" ] || die "Upgrade stopped: the backup has no $file. Nothing was upgraded."
done
[ "$(manifest_field "$backup_dir/manifest.txt" key_fingerprint)" = "$fingerprint" ] \
  && [ "$(manifest_field "$backup_dir/manifest.txt" app_commit)" = "$old_commit" ] \
  || die "Upgrade stopped: $backup_dir/manifest.txt does not match this installation. Nothing was upgraded."
(
  umask 077
  printf 'from_commit=%s\nfrom_helix_image=%s\nto_commit=%s\nto_helix_image=%s\n' \
    "$old_commit" "$old_image" "$new_commit" "$new_image" > "$backup_dir/upgrade.txt"
)

echo "Updating $old_commit to $new_commit."
git merge --ff-only --quiet "$new_commit" \
  || die "Upgrade stopped: git could not fast-forward to $new_commit. Nothing was upgraded."
profile=""
if ! grep -Eq '^MODEL_ENDPOINT=.' .env && [ -n "$(docker compose --profile local ps -a -q ollama 2> /dev/null || true)" ]; then
  profile="--profile local"
fi
echo "Rebuilding and starting: docker compose ${profile:+$profile }up -d --build"
docker compose $profile up -d --build \
  || die "The rebuild failed. Fix the error and run the command above again, or go back: git checkout $old_commit, then scripts/restore.sh \"$backup_dir\" --replace"
port=$(sed -n 's/^PUBLIC_PORT=//p' .env | tail -n 1)
cat <<DONE
Upgraded to $new_commit. The backup from before the upgrade is in $backup_dir.
Wait until this returns {"ok":true,...}, which can take a few minutes:
  curl -fsS http://localhost:${port:-3001}/api/health
To go back: git checkout $old_commit, then scripts/restore.sh "$backup_dir" --replace
DONE
