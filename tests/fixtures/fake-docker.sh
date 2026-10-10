#!/usr/bin/env bash
# Stands in for docker in tests/scripts.test.ts. It appends each call to $STUB_LOG,
# writes a placeholder archive for each volume backup, and keeps each archive a
# restore sends in $STUB_RESTORED. STUB_HELIX_IMAGE is the image of the HelixDB
# container, STUB_RUNNING lists the running services (default: all four),
# STUB_OLLAMA makes an ollama container exist, STUB_FAIL is a pattern of calls that
# fail, STUB_DOWN fails every call as if the Docker daemon were stopped, and
# STUB_DAMAGED makes a volume backup succeed with an archive that cannot be read. With
# STUB_VOLUMES, a volume backup archives the folder named after the volume in
# STUB_VOLUMES (such as helix-foundry_foundry-data) with the script's own tar
# options; this machine's tar stands in for the image's BusyBox tar. A backup
# passphrase in its environment is logged too, so the tests catch the leak.
printf '%s\n' "$*" >> "$STUB_LOG"
if [ -n "${BACKUP_PASSPHRASE+x}${HF_BACKUP_PASSPHRASE+x}" ]; then
  echo 'a backup passphrase is in the environment' >> "$STUB_LOG"
fi
if [ -n "${STUB_DOWN:-}" ]; then
  echo 'Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?' >&2
  exit 1
fi
if [ -n "${STUB_FAIL:-}" ]; then
  case "$*" in $STUB_FAIL)
    echo "fake docker: $* failed" >&2
    exit 1
    ;;
  esac
fi
volume="" source=""
for arg in "$@"; do
  case $arg in
    *_helix-data:/data*) volume=helix-data source=${arg%%:*} ;;
    *_foundry-data:/data*) volume=foundry-data source=${arg%%:*} ;;
  esac
done
case " $* " in
  " version "*) echo 27.0.0 ;;
  " compose "*"ps -a -q helix ") [ -z "${STUB_HELIX_IMAGE:-}" ] || echo stub-helix ;;
  " compose "*"ps -a -q ollama ") [ -z "${STUB_OLLAMA:-}" ] || echo stub-ollama ;;
  " compose ps --services --status running ")
    for service in ${STUB_RUNNING-app worker executor helix}; do echo "$service"; done
    ;;
  " compose "*) ;;
  " inspect "*) echo "${STUB_HELIX_IMAGE:-}" ;;
  " volume inspect "*) ;;
  " run "*)
    case " $* " in
      *" tar czf - "*)
        if [ -n "${STUB_DAMAGED:-}" ]; then
          echo 'not a gzip archive'
          exit 0
        fi
        if [ -n "${STUB_VOLUMES:-}" ]; then
          args=() image=""
          for arg in "$@"; do
            if [ -n "$image" ]; then
              [ "$arg" != /data ] || arg="$STUB_VOLUMES/$source"
              args+=("$arg")
            fi
            [ "$arg" != alpine:3.22 ] || image=1
          done
          exec "${args[@]}"
        fi
        dir=$(mktemp -d)
        echo "placeholder $volume" > "$dir/$volume.txt"
        tar czf - -C "$dir" .
        rm -rf "$dir"
        ;;
      *"tar xzf -"*) cat > "$STUB_RESTORED/$volume.tgz" ;;
      *) exit 1 ;;
    esac
    ;;
  *)
    echo "fake docker: unexpected call: $*" >&2
    exit 1
    ;;
esac
