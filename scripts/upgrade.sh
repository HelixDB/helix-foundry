#!/usr/bin/env bash
# Upgrades this installation. It takes and checks a fresh backup first, then
# fast-forwards the checkout to its upstream branch, rebuilds and restarts. If the
# HelixDB image changes, which can be one way, it asks first (or takes --yes). Until
# the rebuild succeeds, the git directory keeps where the upgrade started and the
# backup taken before it, so the next run finishes the upgrade and names that backup.
set -euo pipefail
here=$PWD
cd "$(dirname "$0")/.."
. scripts/backup-lib.sh
take_passphrase
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
# An upgrade that did not finish, and the backup taken before it. Until it finishes,
# a run that stops says so instead of saying that nothing changed.
pending=$(pending_file)
from_commit=$(pending_field from_commit)
first_backup="" unfinished=""
if [ -n "$from_commit" ]; then
  first_backup=$(pending_field backup)
  unfinished="The earlier upgrade has still not finished, so the checkout and HelixDB may already be partly upgraded.${first_backup:+ $first_backup holds the backup from before it.}"
  unchanged="This run changed nothing. $unfinished"
fi
upstream=$(git rev-parse --abbrev-ref --symbolic-full-name '@{u}' 2> /dev/null) \
  || die 'This branch has no upstream to upgrade from. Switch to the release branch, for example: git switch main'
[ -z "$(git status --porcelain --untracked-files=no)" ] \
  || die "This checkout has uncommitted changes. Commit or stash them first. $unchanged"
head=$(git rev-parse HEAD)
# Before anything reads the HelixDB container, which Docker must be running to show.
docker_ready "$unchanged"
running=$(running_helix_image)
echo "Fetching $upstream."
git fetch --quiet || die "Could not fetch $upstream. $unchanged"
to_commit=$(git rev-parse '@{u}')
to_image=$(git show "$to_commit:compose.yaml" | helix_image)
[ "$to_commit" = "$head" ] || git merge-base --is-ancestor HEAD "$to_commit" \
  || die "This checkout has commits that are not on $upstream, so it cannot fast-forward. $unchanged"

# Where the upgrade starts: an upgrade that did not finish, else the commit
# upgrade.sh or restore.sh last started, else the checkout, which runs after an
# install: when git has not moved it since the clone, or when it is behind the
# upstream and pins the HelixDB image that runs. A checkout pulled by hand may not
# run yet, so otherwise where the upgrade starts is unknown.
if [ -n "$from_commit" ]; then
  from_image=$(pending_field from_helix_image)
  case $from_commit in
    unknown) echo 'An earlier upgrade did not finish, so upgrade.sh finishes it.' ;;
    *) echo "The upgrade from $from_commit did not finish, so upgrade.sh finishes it." ;;
  esac
else
  if from_commit=$(recorded_commit); then
    :
  elif { [ "$to_commit" != "$head" ] || git reflog -n 1 --format=%gs HEAD 2> /dev/null | grep -q '^clone:'; } \
    && { [ -z "$running" ] || same_image "$running" "$(helix_image < compose.yaml)"; }; then
    from_commit=$head
  else
    from_commit=unknown
  fi
  # What runs is the image of the HelixDB container, when there is one.
  from_image=$(file_at "$from_commit" compose.yaml | helix_image)
  if [ -n "$running" ] && ! same_image "$running" "$from_image"; then from_image=$running; fi
  if [ "$from_commit" = "$head" ] && [ "$to_commit" = "$head" ] && same_image "$from_image" "$to_image"; then
    echo "Already up to date at $head."
    exit 0
  fi
  if [ "$to_commit" = "$head" ]; then
    echo "The checkout is up to date at $head, but the services may not run it yet, so upgrade.sh finishes the upgrade."
  fi
  if [ "$from_commit" = unknown ]; then
    echo 'Nothing records which commit the services run, so upgrade.sh cannot name the commit to go back to.'
  fi
fi
# How to go back to where the upgrade started, with the backup in $1. It restores
# with the copy of restore.sh in the backup, because the old release's own
# restore.sh may predate this backup format (an older one needs config.env).
rollback() {
  local restore="bash \"$1/restore.sh\" \"$1\" --replace"
  if [ -d "$1" ] && [ ! -f "$1/restore.sh" ]; then restore="scripts/restore.sh \"$1\" --replace"; fi
  if [ "$from_commit" = unknown ]; then
    echo "find the commit that ran before in git reflog (after a git pull, ORIG_HEAD is usually that commit), run git reset --keep COMMIT, then $restore"
  else
    echo "git reset --keep $from_commit, then $restore"
  fi
}
# The data may already be as new as the HelixDB that runs: an upgrade that did not
# finish can have started the newer image before it failed.
to_version=$(helix_version "$to_image")
for opened in "$from_image" "$running"; do
  version=$(helix_version "$opened")
  if [ -n "$version" ] && [ -n "$to_version" ] && version_newer "$version" "$to_version"; then
    die "$to_commit moves HelixDB back from $version to $to_version. An older HelixDB may refuse data a newer one opened, so upgrade.sh does not start it. Stay on the release that runs now, and ask the Helix Foundry maintainers which release to use. $unchanged"
  fi
done
if [ -z "$from_image" ] || ! same_image "$from_image" "$to_image"; then
  cat >&2 <<WARNING
WARNING: this upgrade changes the HelixDB image
  from ${from_image:-an image upgrade.sh cannot tell}
  to   $to_image
HelixDB upgrades its storage one way the first time a newer version opens it: from
v0.0.10 on it upgrades the index storage, and v0.0.9 and earlier then refuse the
database. Once it starts, only the backup taken before the upgrade can take the
installation back: $(rollback "${first_backup:-$backup_dir}").
WARNING
  if [ -n "$running" ] && [ -n "$from_image" ] && ! same_image "$running" "$from_image"; then
    echo "HelixDB $running already runs, so it may have opened the data already." >&2
  fi
  if [ -z "$yes" ]; then
    [ -t 0 ] || die "Run again with --yes to accept the HelixDB upgrade. $unchanged"
    printf 'Type yes to upgrade HelixDB: ' >&2
    read -r answer
    [ "$answer" = yes ] || die "Upgrade cancelled. $unchanged"
  fi
fi

# Recorded before the backup, so the backup names the commit the upgrade starts from.
write_pending() {
  (
    umask 077
    printf 'from_commit=%s\nfrom_helix_image=%s\nbackup=%s\n' \
      "$from_commit" "$from_image" "$first_backup" > "$pending"
  )
}
new_pending=""
if [ -z "$(pending_field from_commit)" ]; then
  write_pending
  new_pending=1
fi
# Stops before this run upgrades anything. A retry stops with the earlier upgrade
# still unfinished, so it says so and keeps its record.
stop() {
  if [ -n "$new_pending" ]; then
    rm -f "$pending"
    die "Upgrade stopped: $*. Nothing was upgraded."
  fi
  die "Upgrade stopped: $*. $unfinished Fix the error, then run scripts/upgrade.sh again, which finishes the upgrade${first_backup:+, or go back: $(rollback "$first_backup")}."
}
# backup.sh gets BACKUP_PASSPHRASE back for --include-key, and nothing else does.
run_backup() (
  { set +x; } 2> /dev/null
  if [ -n "$include_key" ] && [ -n "$passphrase" ]; then export BACKUP_PASSPHRASE=$passphrase; fi
  "$BASH" scripts/backup.sh $include_key "$backup_dir"
)
echo "Backing up to $backup_dir before upgrading."
status=0
run_backup || status=$?
# Status 3: the backup is complete, and the rebuild below starts the services.
[ "$status" = 0 ] || [ "$status" = 3 ] || stop 'the backup failed'
backup_dir="$(cd "$backup_dir" && pwd -P)"
for file in helix-data.tgz foundry-data.tgz restore.sh backup-lib.sh manifest.txt; do
  [ -s "$backup_dir/$file" ] || stop "the backup has no $file"
done
[ "$(manifest_field "$backup_dir/manifest.txt" key_fingerprint)" = "$fingerprint" ] \
  && [ "$(manifest_field "$backup_dir/manifest.txt" app_commit)" = "$from_commit" ] \
  || stop "$backup_dir/manifest.txt does not match this installation"
(
  umask 077
  printf 'from_commit=%s\nfrom_helix_image=%s\nto_commit=%s\nto_helix_image=%s\n' \
    "$from_commit" "$from_image" "$to_commit" "$to_image" > "$backup_dir/upgrade.txt"
)
# The first complete backup is the one from before the upgrade.
if [ -z "$first_backup" ] || [ ! -s "$first_backup/manifest.txt" ]; then first_backup=$backup_dir; fi
write_pending
back=$(rollback "$first_backup")
after=""
if [ "$first_backup" != "$backup_dir" ]; then
  after="This run also backed up to $backup_dir."
  if [ -n "$running" ] && [ -n "$from_image" ] && ! same_image "$running" "$from_image"; then
    after="$after HelixDB $running had already opened the data, so only $first_backup can go back."
  fi
fi

if [ "$to_commit" != "$head" ]; then
  echo "Updating $head to $to_commit."
  git merge --ff-only --quiet "$to_commit" || stop "git could not fast-forward to $to_commit"
fi
profile=""
if [ -z "$(env_value MODEL_ENDPOINT < .env)" ] \
  && [ -n "$(docker compose --profile local ps -a -q ollama 2> /dev/null || true)" ]; then
  profile="--profile local"
fi
echo "Rebuilding and starting: docker compose ${profile:+$profile }up -d --build"
docker compose $profile up -d --build \
  || die "The rebuild failed. $first_backup holds the backup from before the upgrade. Fix the error and run scripts/upgrade.sh again, which finishes the upgrade, or go back: $back"
record_deployed
rm -f "$pending"
port=$(env_value PUBLIC_PORT < .env)
cat <<DONE
Upgraded to $to_commit. The backup from before the upgrade is in $first_backup.${after:+
$after}
Wait until this returns {"ok":true,...}, which can take a few minutes:
  curl -fsS http://localhost:${port:-3001}/api/health
To go back: $back
DONE
