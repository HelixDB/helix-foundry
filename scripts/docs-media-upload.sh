#!/bin/sh
# Publishes what `pnpm docs:media` captured (DOCS_MEDIA_DIR, docs-media/ by
# default) to the public docs' media CDN, https://assets.helix-db.com/docs/foundry/:
# the S3 bucket DOCS_MEDIA_BUCKET, which only the CloudFront distribution
# DOCS_MEDIA_DISTRIBUTION reads. Needs AWS credentials that can write the
# bucket and invalidate the distribution. Nothing is deleted: a capture of
# one spec replaces only its own files.
set -eu
DIR="${DOCS_MEDIA_DIR:-docs-media}"
BUCKET="${DOCS_MEDIA_BUCKET:?set DOCS_MEDIA_BUCKET}"
PREFIX=docs/foundry
DISTRIBUTION="${DOCS_MEDIA_DISTRIBUTION:?set DOCS_MEDIA_DISTRIBUTION}"
if [ ! -d "$DIR" ]; then
  echo "No $DIR: run pnpm docs:media first." >&2
  exit 1
fi
aws s3 sync "$DIR" "s3://$BUCKET/$PREFIX" --region us-east-1 \
  --exclude "*" --include "*.png" --include "*.webm" \
  --cache-control "public, max-age=3600"
aws cloudfront create-invalidation --distribution-id "$DISTRIBUTION" \
  --paths "/$PREFIX/*" --query "Invalidation.Id" --output text >/dev/null
echo "Published to https://assets.helix-db.com/$PREFIX/"
