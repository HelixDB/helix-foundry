#!/bin/sh
# pnpm dev: https://local.foundry.helix-db.com:3000 against the WFE named in
# .env.local (the dev stamp by default). Needs mkcert and this line in
# /etc/hosts: 127.0.0.1 local.foundry.helix-db.com
set -eu
host=local.foundry.helix-db.com
if [ ! -f "./$host.pem" ] || [ ! -f "./$host-key.pem" ]; then
  mkcert -install && mkcert "$host"
fi
NEXT_PUBLIC_WORKOS_REDIRECT_URI="https://$host:3000/login/callback" exec next dev \
  --hostname "$host" --port 3000 --experimental-https \
  --experimental-https-key "./$host-key.pem" --experimental-https-cert "./$host.pem"
