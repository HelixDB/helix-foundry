#!/usr/bin/env bash
# Upgrades this installation. It takes and checks a fresh backup first, then
# fast-forwards the checkout to its upstream branch, rebuilds and restarts. If the
# HelixDB image changes, which can be one way, it asks first (or takes --yes). When
# the services do not run the checked-out release yet, for example after a failed
# rebuild, it finishes the upgrade the same way.
set -euo pipefail
here=$PWD
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
[ -n "$backup_dir" ] || backup_dir=$(new_backup_dir)
backup_dir=$(from_here "$backup_dir")
unchanged='Nothing was changed.'
[ -f .env ] || die 'There is no .env here. upgrade.sh upgrades an existing installation; see AGENTS.md to install.'
fingerprint=$(key_fingerprint < .env)
[ -n "$fingerprint" ] || die 'ENCRYPTION_KEY in .env is missing or shorter than 32 characters.'
upstream=$(git rev-parse --abbrev-ref --symbolic-full-name '@{u}' 2> /dev/null) \
  || die 'This branch has no upstream to upgrade from. Switch to the release branch, for example: git switch main'
[ -z "$(git status --porcelain --untracked-files=no)" ] \
  || die "This checkout has uncommitted changes. Commit or stash them first. $unchanged"
head=$(git rev-parse HEAD)
# What runs now: the commit upgrade.sh or restore.sh last started, and the image of
# the HelixDB container. Without them, assume the checked-out commit's.
from_commit=$(recorded_commit) || from_commit=""
from_image=$(running_helix_image)
[ -n "$from_image" ] || from_image=$(file_at "${from_commit:-$head}" compose.yaml | helix_image)
echo "Fetching $upstream."
git fetch --quiet || die "Could not fetch $upstream. $unchanged"
to_commit=$(git rev-parse '@{u}')
to_image=$(git show "$to_commit:compose.yaml" | helix_image)
if [ "$to_commit" = "$head" ]; then
  if [ "$from_commit" = "$head" ] && [ "$from_image" = "$to_image" ]; then
    echo "Already up to date at $head."
    exit 0
  fi
  echo "The checkout is up to date at $head, but the services may not run it yet, so upgrade.sh finishes the upgrade."
else
  git merge-base --is-ancestor HEAD "$to_commit" \
    || die "This checkout has commits that are not on $upstream, so it cannot fast-forward. $unchanged"
fi
from_commit=${from_commit:-$head}
back="git reset --keep $from_commit, then scripts/restore.sh \"$backup_dir\" --replace"
from_version=$(helix_version "$from_image")
to_version=$(helix_version "$to_image")
if [ -n "$from_version" ] && [ -n "$to_version" ] && version_newer "$from_version" "$to_version"; then
  die "$to_commit moves HelixDB back from $from_version to $to_version. An older HelixDB may refuse data a newer one opened, so upgrade.sh does not start it. Stay on the release that runs now, and ask the Helix Foundry maintainers which release to use. $unchanged"
fi
if [ "$to_image" != "$from_image" ]; then
  cat >&2 <<WARNING
WARNING: this upgrade changes the HelixDB image
  from $from_image
  to   $to_image
HelixDB upgrades its storage one way the first time a newer version opens it: from
v0.0.10 on it upgrades the index storage, and v0.0.9 and earlier then refuse the
database. Once it starts, going back to $from_commit needs the backup
this script takes first: $back.
WARNING
  if [ -z "$yes" ]; then
    [ -t 0 ] || die "Run again with --yes to accept the HelixDB upgrade. $unchanged"
    printf 'Type yes to upgrade HelixDB: ' >&2
    read -r answer
    [ "$answer" = yes ] || die "Upgrade cancelled. $unchanged"
  fi
fi

echo "Backing up to $backup_dir before upgrading."
status=0
"$BASH" scripts/backup.sh $include_key "$backup_dir" || status=$?
# Status 3: the backup is complete, and the rebuild below starts the services.
[ "$status" = 0 ] || [ "$status" = 3 ] || die 'Upgrade stopped: the backup failed. Nothing was upgraded.'
backup_dir="$(cd "$backup_dir" && pwd -P)"
back="git reset --keep $from_commit, then scripts/restore.sh \"$backup_dir\" --replace"
for file in helix-data.tgz foundry-data.tgz manifest.txt; do
  [ -s "$backup_dir/$file" ] || die "Upgrade stopped: the backup has no $file. Nothing was upgraded."
done
[ "$(manifest_field "$backup_dir/manifest.txt" key_fingerprint)" = "$fingerprint" ] \
  && [ "$(manifest_field "$backup_dir/manifest.txt" app_commit)" = "$from_commit" ] \
  || die "Upgrade stopped: $backup_dir/manifest.txt does not match this installation. Nothing was upgraded."
(
  umask 077
  printf 'from_commit=%s\nfrom_helix_image=%s\nto_commit=%s\nto_helix_image=%s\n' \
    "$from_commit" "$from_image" "$to_commit" "$to_image" > "$backup_dir/upgrade.txt"
)

# Until the rebuild succeeds, the services run $from_commit.
record_deployed "$from_commit"
if [ "$to_commit" != "$head" ]; then
  echo "Updating $head to $to_commit."
  git merge --ff-only --quiet "$to_commit" \
    || die "Upgrade stopped: git could not fast-forward to $to_commit. Nothing was upgraded."
fi
profile=""
if ! grep -Eq '^MODEL_ENDPOINT=.' .env && [ -n "$(docker compose --profile local ps -a -q ollama 2> /dev/null || true)" ]; then
  profile="--profile local"
fi
echo "Rebuilding and starting: docker compose ${profile:+$profile }up -d --build"
docker compose $profile up -d --build \
  || die "The rebuild failed. $backup_dir holds the backup from before the upgrade. Fix the error and run scripts/upgrade.sh again, or go back: $back"
record_deployed
port=$(sed -n 's/^PUBLIC_PORT=//p' .env | tail -n 1)
cat <<DONE
Upgraded to $to_commit. The backup from before the upgrade is in $backup_dir.
Wait until this returns {"ok":true,...}, which can take a few minutes:
  curl -fsS http://localhost:${port:-3001}/api/health
To go back: $back
DONE
