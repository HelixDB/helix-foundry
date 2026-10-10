#!/usr/bin/env bash
# Stands in for docker in tests/scripts.test.ts. It appends each call to $STUB_LOG,
# writes a placeholder archive for each volume backup, and keeps each archive a
# restore sends in $STUB_RESTORED. STUB_HELIX_IMAGE is the running HelixDB image,
# STUB_OLLAMA makes an ollama container exist, and STUB_FAIL_RUN fails docker run.
printf '%s\n' "$*" >> "$STUB_LOG"
volume=""
for arg in "$@"; do
  case $arg in
    *_helix-data:/data*) volume=helix-data ;;
    *_foundry-data:/data*) volume=foundry-data ;;
  esac
done
case " $* " in
  " compose "*"ps -a -q helix ") [ -z "${STUB_HELIX_IMAGE:-}" ] || echo stub-helix ;;
  " compose "*"ps -a -q ollama ") [ -z "${STUB_OLLAMA:-}" ] || echo stub-ollama ;;
  " compose "*) ;;
  " inspect "*) echo "${STUB_HELIX_IMAGE:-}" ;;
  " volume inspect "*) ;;
  " run "*)
    [ -z "${STUB_FAIL_RUN:-}" ] || { echo 'fake docker: run failed' >&2; exit 1; }
    case " $* " in
      *" tar czf - "*)
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
