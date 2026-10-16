#!/usr/bin/env bash
# Full nft TPROXY datapath, isolated behind a software VM with no external NIC.
set -euo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ "$(uname -m)" != x86_64 ]]; then
  echo 'This software VM regression currently requires a Linux amd64 runner.' >&2
  exit 1
fi
scratch="$(mktemp -d)"
fixture_name="incudal-datapath-egress-$$"
fixture_image="${fixture_name}:test"
cleanup() {
  docker rm -f "$fixture_name" >/dev/null 2>&1 || true
  docker image rm "$fixture_image" >/dev/null 2>&1 || true
  rm -rf "$scratch"
}
trap cleanup EXIT
python3 "$ROOT_DIR/scripts/prepare-xray.py" amd64
(cd "$ROOT_DIR"; CGO_ENABLED=0 go test -c -o "$scratch/egress.test" ./internal/egress)
cp "$ROOT_DIR/scripts/fixtures/egress-datapath.Dockerfile" "$scratch/Dockerfile"
cp "$ROOT_DIR/scripts/fixtures/egress-datapath-init" "$scratch/egress-datapath-init"
docker build -t "$fixture_image" "$scratch"
# All NET_ADMIN/SYS_ADMIN operations happen inside the guest kernel. Docker
# receives no capabilities, host namespace, device, or host filesystem mount.
docker run --rm --name "$fixture_name" --network none --cap-drop ALL \
  --security-opt no-new-privileges --memory 2g --cpus 2 "$fixture_image" \
  | tee "$scratch/run.log"
fixture_log="$(cat "$scratch/run.log")"
if [[ "$fixture_log" != *INCUDAL_DATAPATH_EXIT=0* ]]; then
  echo 'Full-kernel transparent egress regression failed.' >&2
  exit 1
fi
echo 'Full-kernel transparent egress regression passed.'
