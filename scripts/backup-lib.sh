# Helpers for backup.sh, restore.sh and upgrade.sh, sourced from the repository
# root. Nothing here prints ENCRYPTION_KEY or INTERNAL_KEY.

# The .env lines a backup keeps in settings.env. Keys and client secrets stay out.
settings_pattern='^(PUBLIC_PORT|PUBLIC_ORIGIN|MODEL_ENDPOINT|COMPOSE_PROJECT_NAME|APP_ORIGIN|HELIX_URL|EXECUTOR_URL|OLLAMA_URL|DATA_DIR|PORT|EXECUTOR_PORT|NEON_CLIENT_ID|SUPABASE_CLIENT_ID|PLANETSCALE_CLIENT_ID)='
# PBKDF2 iterations for env.enc, the passphrase-encrypted .env (backup.sh --include-key).
key_iterations=600000

die() {
  echo "$*" >&2
  exit 1
}

# Reads an env file on stdin and prints a fingerprint of its ENCRYPTION_KEY, or
# nothing if the key is missing or shorter than 32 characters. The app encrypts
# credentials with sha256(ENCRYPTION_KEY), so the fingerprint hashes the key with a
# prefix: it identifies the key without revealing anything that decrypts.
key_fingerprint() {
  local key
  key=$(sed -n 's/^ENCRYPTION_KEY=//p' | tail -n 1 | tr -d '\r')
  case $key in \"*\" | \'*\') key=${key:1:${#key}-2} ;; esac
  if [ "${#key}" -ge 32 ]; then
    printf 'helix-foundry-key-fingerprint:%s' "$key" | openssl dgst -sha256 | awk '{print $NF}'
  fi
}

# Prints field $2 of the manifest file $1, or nothing.
manifest_field() {
  if [ -f "$1" ]; then sed -n "s/^$2=//p" "$1" | tail -n 1; fi
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

# Prints the metadata schema version of this checkout's application.
schema_version() {
  if [ -f apps/api/src/store.ts ]; then
    sed -n 's/^export const SCHEMA_VERSION = \([0-9][0-9]*\);.*/\1/p' apps/api/src/store.ts
  fi
}

# Sets $passphrase from BACKUP_PASSPHRASE or a terminal prompt; "new" asks twice.
read_passphrase() {
  passphrase=${BACKUP_PASSPHRASE:-}
  [ -z "$passphrase" ] || return 0
  [ -t 0 ] || die 'Set BACKUP_PASSPHRASE, or run this in a terminal to type the backup passphrase.'
  IFS= read -rs -p 'Backup passphrase: ' passphrase
  echo >&2
  if [ "${1:-}" = new ]; then
    local again
    IFS= read -rs -p 'Type it again: ' again
    echo >&2
    [ "$passphrase" = "$again" ] || die 'The passphrases do not match.'
  fi
}

# The passphrase reaches openssl through its environment, never its arguments.
encrypt_env() { # $1 = env file, $2 = output
  HF_BACKUP_PASSPHRASE=$passphrase openssl enc -aes-256-cbc -salt -pbkdf2 -md sha256 \
    -iter "$key_iterations" -pass env:HF_BACKUP_PASSPHRASE -in "$1" -out "$2"
}
decrypt_env() { # $1 = env.enc, $2 = iterations; writes the .env to stdout
  HF_BACKUP_PASSPHRASE=$passphrase openssl enc -d -aes-256-cbc -pbkdf2 -md sha256 \
    -iter "${2:-$key_iterations}" -pass env:HF_BACKUP_PASSPHRASE -in "$1"
}

# Prints path $1 from the command line as an absolute path. The scripts run from
# the repository root, so relative paths start from $here, where the user was.
from_here() {
  case $1 in
    /*) printf '%s\n' "$1" ;;
    *) printf '%s\n' "$here/$1" ;;
  esac
}

# Succeeds when the absolute path $1 is inside this repository.
inside_repo() {
  case "$1/" in "$(pwd -P)/"*) return 0 ;; esac
  return 1
}
