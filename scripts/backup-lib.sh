# Helpers for backup.sh, restore.sh and upgrade.sh, sourced from the repository
# root. Nothing here prints ENCRYPTION_KEY or INTERNAL_KEY, and the functions that
# handle a key or a passphrase turn off tracing (bash -x), which would print them.

# The .env lines a backup keeps in settings.env. Keys and client secrets stay out.
settings_pattern='^[[:space:]]*(export[[:space:]]+)?(PUBLIC_PORT|PUBLIC_ORIGIN|MODEL_ENDPOINT|COMPOSE_PROJECT_NAME|APP_ORIGIN|HELIX_URL|EXECUTOR_URL|OLLAMA_URL|DATA_DIR|PORT|EXECUTOR_PORT|NEON_CLIENT_ID|SUPABASE_CLIENT_ID|PLANETSCALE_CLIENT_ID)[[:space:]]*='
# PBKDF2 iterations for env.enc, the passphrase-encrypted .env (backup.sh --include-key).
key_iterations=600000

die() {
  echo "$*" >&2
  exit 1
}

# Prints the value of $1 in an env file read on stdin, as Compose reads it: the last
# line wins, "export " and spaces around = are allowed, quotes are removed, and an
# unquoted value ends before " #". Callers that read a key turn off tracing first.
env_value() {
  local line value
  line=$(tr -d '\r' | sed -n "s/^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}$1[[:space:]]*=[[:space:]]*//p" | tail -n 1)
  case $line in
    \"*) value=${line#\"}; value=${value%%\"*} ;;
    \'*) value=${line#\'}; value=${value%%\'*} ;;
    *)
      value=${line%%[[:space:]]#*}
      value=${value%"${value##*[![:space:]]}"}
      ;;
  esac
  printf '%s' "$value"
}

# Reads an env file on stdin and prints a fingerprint of its ENCRYPTION_KEY, or
# nothing if the key is missing or shorter than 32 characters. The app encrypts
# credentials with sha256(ENCRYPTION_KEY), so the fingerprint hashes the key with a
# prefix: it identifies the key without revealing anything that decrypts.
key_fingerprint() (
  { set +x; } 2> /dev/null
  key=$(env_value ENCRYPTION_KEY)
  if [ "${#key}" -ge 32 ]; then
    printf 'helix-foundry-key-fingerprint:%s' "$key" | openssl dgst -sha256 | awk '{print $NF}'
  fi
)

# Succeeds when key $1 in the env file read on stdin has at least 32 characters.
key_set() (
  { set +x; } 2> /dev/null
  key=$(env_value "$1")
  [ "${#key}" -ge 32 ]
)

# Prints field $2 of the manifest file $1, or nothing.
manifest_field() {
  if [ -f "$1" ]; then sed -n "s/^$2=//p" "$1" | tail -n 1; fi
}

# Prints the Compose project name, which names the volumes, as Compose finds it:
# COMPOSE_PROJECT_NAME from the shell, else from env file $1 (default .env), else
# the top-level name in compose.yaml.
compose_project() {
  local name=${COMPOSE_PROJECT_NAME:-} file=${1:-.env}
  if [ -z "$name" ] && [ -f "$file" ]; then name=$(env_value COMPOSE_PROJECT_NAME < "$file"); fi
  if [ -z "$name" ] && [ -f compose.yaml ]; then
    name=$(sed -n "s/^name:[[:space:]]*[\"']\{0,1\}\([A-Za-z0-9_-]*\).*/\1/p" compose.yaml | head -n 1)
  fi
  printf '%s\n' "${name:-helix-foundry}"
}

# Prints the image of the helix service in a compose file read on stdin.
helix_image() {
  awk '/^  [A-Za-z0-9_-]+:/ { helix = ($1 == "helix:") }
    helix && $1 == "image:" && !found { print $2; found = 1 }'
}

# Prints X.Y.Z from an image such as ghcr.io/helixdb/helixdb:v0.0.6@sha256:...
helix_version() {
  printf '%s\n' "$1" | sed -n 's/^[^@]*:v\{0,1\}\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\).*/\1/p'
}

# Prints the digest (sha256:...) an image reference names, or nothing.
image_digest() {
  case $1 in *@sha256:*) printf 'sha256:%s\n' "${1##*@sha256:}" ;; esac
}

# Succeeds when images $1 and $2 are the same HelixDB release. Docker can record the
# image of a container without the tag or the digest compose.yaml gives, so this
# compares the digests when both have one, and the versions otherwise.
same_image() {
  local a b
  [ "$1" != "$2" ] || return 0
  a=$(image_digest "$1") b=$(image_digest "$2")
  if [ -n "$a" ] && [ -n "$b" ]; then
    [ "$a" = "$b" ]
    return
  fi
  a=$(helix_version "$1") b=$(helix_version "$2")
  [ -n "$a" ] && [ "$a" = "$b" ]
}

# Succeeds when version $1 is newer than version $2 (both X.Y.Z).
version_newer() {
  local a=$1 b=$2 x y
  while [ -n "$a$b" ]; do
    x=${a%%.*} y=${b%%.*}
    if [ "${x:-0}" -ne "${y:-0}" ]; then
      [ "${x:-0}" -gt "${y:-0}" ]
      return
    fi
    case $a in *.*) a=${a#*.} ;; *) a= ;; esac
    case $b in *.*) b=${b#*.} ;; *) b= ;; esac
  done
  return 1
}

# Prints the index storage version HelixDB $1 (X.Y.Z) uses, where the release notes
# give it: v0.0.6 to v0.0.9 keep one data format, and v0.0.10 upgrades it one way
# to the format v0.0.11 also uses. Prints nothing for other versions.
helix_storage() {
  case $1 in
    0.0.[6-9]) echo 4 ;;
    0.0.1[01]) echo 5 ;;
  esac
}

# Prints the metadata schema version from apps/api/src/store.ts read on stdin.
schema_version() {
  sed -n 's/^export const SCHEMA_VERSION = \([0-9][0-9]*\);.*/\1/p'
}

# Prints file $2 as of commit $1: this checkout's copy when $1 is checked out or
# there is no git, and nothing when git cannot show it or $1 is "unknown".
file_at() {
  local head
  head=$(git rev-parse HEAD 2> /dev/null) || head=""
  if [ -z "$head" ] || [ "$1" = "$head" ]; then
    cat "$2" 2> /dev/null || true
  elif [ "$1" != unknown ]; then
    git show "$1:$2" 2> /dev/null || true
  fi
}

# upgrade.sh and restore.sh record the commit the services run in the git
# directory, so it is never committed. recorded_commit prints it, or fails when
# there is no record; record_deployed records the checked-out commit.
deployed_file() { git rev-parse --git-path helix-foundry-deployed 2> /dev/null; }
recorded_commit() {
  local file
  file=$(deployed_file) && [ -s "$file" ] || return 1
  git rev-parse --verify --quiet "$(head -n 1 "$file")^{commit}"
}
record_deployed() {
  local file
  file=$(deployed_file) && git rev-parse HEAD 2> /dev/null > "$file" || true
}

# While an upgrade has not finished, upgrade.sh keeps in the git directory where it
# started (from_commit, "unknown" when no record said, and from_helix_image) and the
# backup taken before it (backup). restore.sh clears it.
pending_file() { git rev-parse --git-path helix-foundry-upgrade 2> /dev/null; }
pending_field() {
  local file
  file=$(pending_file) || return 0
  manifest_field "$file" "$1"
}
clear_pending() {
  local file
  file=$(pending_file) && rm -f "$file" || true
}

# Prints the commit the services were started from: where an unfinished upgrade
# started, else the commit upgrade.sh or restore.sh last started, else the checkout.
deployed_commit() {
  local commit
  commit=$(pending_field from_commit)
  if [ -n "$commit" ]; then
    printf '%s\n' "$commit"
  else
    recorded_commit || git rev-parse HEAD 2> /dev/null || echo unknown
  fi
}

# Prints the image the HelixDB container was created from, or nothing.
running_helix_image() {
  local container
  container=$(docker compose ps -a -q helix 2> /dev/null | head -n 1) || true
  if [ -n "$container" ]; then
    docker inspect --format '{{.Config.Image}}' "$container" 2> /dev/null || true
  fi
}

# Stops with Docker's own error when Docker is not running or cannot be used.
docker_ready() {
  local error
  error=$(docker version --format '{{.Server.Version}}' 2>&1 > /dev/null) \
    || die "Docker is not available: ${error:-docker version failed}. Start Docker, then run this again.${1:+ $1}"
}

# Moves BACKUP_PASSPHRASE into $passphrase and out of the environment, so docker,
# git and the other commands these scripts run do not inherit it.
take_passphrase() {
  { local trace=$-; set +x; } 2> /dev/null
  passphrase=${BACKUP_PASSPHRASE:-}
  unset BACKUP_PASSPHRASE
  case $trace in *x*) set -x ;; esac
}

# Sets $passphrase, unless take_passphrase found one, from a terminal prompt; "new"
# asks twice.
read_passphrase() {
  { local trace=$-; set +x; } 2> /dev/null
  if [ -z "$passphrase" ]; then
    [ -t 0 ] || die 'Set BACKUP_PASSPHRASE, or run this in a terminal to type the backup passphrase.'
    IFS= read -rs -p 'Backup passphrase: ' passphrase
    echo >&2
    if [ "${1:-}" = new ]; then
      local again
      IFS= read -rs -p 'Type it again: ' again
      echo >&2
      [ "$passphrase" = "$again" ] || die 'The passphrases do not match.'
    fi
  fi
  case $trace in *x*) set -x ;; esac
}

# The passphrase reaches openssl through its environment, never its arguments.
encrypt_env() ( # $1 = env file, $2 = output
  { set +x; } 2> /dev/null
  HF_BACKUP_PASSPHRASE=$passphrase openssl enc -aes-256-cbc -salt -pbkdf2 -md sha256 \
    -iter "$key_iterations" -pass env:HF_BACKUP_PASSPHRASE -in "$1" -out "$2"
)
decrypt_env() ( # $1 = env.enc, $2 = iterations; writes the .env to stdout
  { set +x; } 2> /dev/null
  HF_BACKUP_PASSPHRASE=$passphrase openssl enc -d -aes-256-cbc -pbkdf2 -md sha256 \
    -iter "${2:-$key_iterations}" -pass env:HF_BACKUP_PASSPHRASE -in "$1"
)

# Warns about backups an older backup.sh made, by default in the repository's
# backups/ folder: their config.env holds ENCRYPTION_KEY and INTERNAL_KEY in plain
# text. It lists the folders and never reads the files.
warn_old_backups() {
  local found count
  found=$(
    { find "$(pwd -P)/backups" "$HOME/helix-foundry-backups" -mindepth 2 -maxdepth 2 \
      -type f -name config.env 2> /dev/null || true; } | sed 's|/config.env$||' | sort
  )
  [ -n "$found" ] || return 0
  count=$(printf '%s\n' "$found" | wc -l | tr -d ' ')
  {
    echo 'WARNING: these backups, made by an older backup.sh, hold the keys in plain text in config.env:'
    printf '%s\n' "$found" | head -n 5 | sed 's/^/  /'
    [ "$count" -le 5 ] || echo "  and $((count - 5)) more"
    echo 'Anyone with a copy can decrypt the stored credentials. Once you have a new backup and'
    echo 'a separate copy of .env, delete those backups, or at least their config.env.'
  } >&2
}

# Prints a new folder for a backup in ~/helix-foundry-backups, named after the time.
new_backup_dir() {
  local base dir n=1
  base="$HOME/helix-foundry-backups/$(date +%Y%m%d-%H%M%S)"
  dir=$base
  while [ -e "$dir" ]; do
    n=$((n + 1))
    dir="$base-$n"
  done
  printf '%s\n' "$dir"
}

# Prints path $1 from the command line as an absolute path. The scripts run from
# the repository root, so relative paths start from $here, where the user was.
from_here() {
  case $1 in
    /*) printf '%s\n' "$1" ;;
    *) printf '%s\n' "$here/$1" ;;
  esac
}

# Succeeds when the absolute path $1, which need not exist yet, is inside this
# repository. It compares folders by device and inode (-ef), so another spelling
# of the path, such as other letter case or a symbolic link, still matches.
inside_repo() {
  local dir=$1
  while [ ! -d "$dir" ]; do dir=$(dirname "$dir"); done
  dir=$(cd "$dir" && pwd -P) || return 0
  while true; do
    if [ "$dir" -ef . ]; then return 0; fi
    if [ "$dir" = / ]; then return 1; fi
    dir=$(dirname "$dir")
  done
}
