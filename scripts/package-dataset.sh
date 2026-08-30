#!/usr/bin/env bash
# Package the built dataset for a GitHub release: zstd-compressed, split
# into chunks under GitHub's 2GB asset limit, with a checksum manifest.
#
#   scripts/package-dataset.sh [dataset.db] [outdir]
#
# Reassembly is documented in the manifest it writes.
set -euo pipefail

DB="${1:-data/dataset/metadata.db}"
OUT="${2:-dist}"
CHUNK="1900M"

[ -f "$DB" ] || { echo "package-dataset: $DB not found" >&2; exit 1; }
command -v zstd >/dev/null || { echo "package-dataset: zstd is required" >&2; exit 1; }

mkdir -p "$OUT"
rm -f "$OUT"/metadata.db.zst* "$OUT"/manifest.txt

echo "compressing $DB"
zstd -T0 -12 -q "$DB" -o "$OUT/metadata.db.zst"

SIZE=$(stat -c %s "$OUT/metadata.db.zst")
LIMIT=$((1900 * 1024 * 1024))
if [ "$SIZE" -gt "$LIMIT" ]; then
    echo "splitting into $CHUNK chunks"
    split -b "$CHUNK" -d "$OUT/metadata.db.zst" "$OUT/metadata.db.zst.part"
    rm "$OUT/metadata.db.zst"
fi

(
    cd "$OUT"
    {
        echo "# readarr-metadata-provider dataset"
        echo "# generated: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
        echo "# uncompressed size: $(stat -c %s "$OLDPWD/$DB") bytes"
        echo "#"
        if ls metadata.db.zst.part* >/dev/null 2>&1; then
            echo "# reassemble with:"
            echo "#   cat metadata.db.zst.part* | zstd -d -o metadata.db"
        else
            echo "# decompress with:"
            echo "#   zstd -d metadata.db.zst -o metadata.db"
        fi
        echo "#"
        sha256sum metadata.db.zst*
    } > manifest.txt
)

echo "packaged:"
ls -lh "$OUT"
