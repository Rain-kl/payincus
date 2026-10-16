#!/usr/bin/env bash
# Real systemd/namespace/native-Xray startup regression in a disposable container.
# No host files, host networking, host PID namespace or host cgroup mounts are used.
set -euo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
case "$(uname -m)" in
  x86_64) native_arch=amd64 ;;
  aarch64|arm64) native_arch=arm64 ;;
  *) echo 'A native Linux amd64/arm64 Docker host is required' >&2; exit 1 ;;
esac
scratch="$(mktemp -d)"
fixture_name="incudal-systemd-egress-$$"
fixture_image="${fixture_name}:test"
cleanup() {
  docker rm -f "$fixture_name" >/dev/null 2>&1 || true
  docker image rm "$fixture_image" >/dev/null 2>&1 || true
  rm -rf "$scratch"
}
trap cleanup EXIT
python3 "$ROOT_DIR/scripts/prepare-xray.py" "$native_arch"
(cd "$ROOT_DIR"; CGO_ENABLED=0 go test -c -o "$scratch/egress.test" ./internal/egress)
docker build -t "$fixture_image" -f "$ROOT_DIR/scripts/fixtures/egress-systemd.Dockerfile" "$ROOT_DIR/scripts/fixtures"
# SYS_ADMIN is needed for isolated mount/net namespaces; NET_* for nftables and
# real TCP/UDP/DNS listeners. systemd requires the listed UID/filesystem caps.
# The writable cgroup mount is the container's OWN private cgroup namespace.
docker run -d -t --name "$fixture_name" \
  --cap-drop ALL --cap-add SYS_ADMIN --cap-add NET_ADMIN --cap-add NET_RAW \
  --cap-add NET_BIND_SERVICE --cap-add SETUID --cap-add SETGID --cap-add CHOWN \
  --cap-add DAC_OVERRIDE --cap-add FOWNER --cap-add SETPCAP --cap-add SYS_CHROOT \
  --cap-add KILL --cap-add AUDIT_WRITE --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
  --cgroupns=private -e container=docker -e SYSTEMD_LOG_TARGET=console \
  --tmpfs /run --tmpfs /tmp "$fixture_image" \
  sh -ec 'mount -o remount,rw /sys/fs/cgroup; exec /sbin/init'
for attempt in {1..60}; do
  if docker exec "$fixture_name" systemctl is-active --quiet dbus.service; then break; fi
  if [[ "$attempt" == 60 ]]; then docker logs --tail 40 "$fixture_name"; exit 1; fi
  sleep 0.2
done
docker exec "$fixture_name" mkdir -p /opt/incudal-tests
docker cp "$scratch/egress.test" "$fixture_name:/opt/incudal-tests/egress.test"
docker exec "$fixture_name" touch /run/incudal-systemd-test-authorized
run_fixture() {
  docker exec "$fixture_name" systemd-run --unit="incudal-agent-fixture-$1" \
    --collect --wait --pipe -p User=root -p NoNewPrivileges=yes -p PrivateTmp=yes \
    -p ProtectHome=yes -p CPUAccounting=yes -p CPUQuota=20% \
    -p MemoryAccounting=yes -p MemoryMax=256M \
    --setenv="INCUDAL_SYSTEMD_NAMESPACE_TEST=$1" \
    /opt/incudal-tests/egress.test -test.run '^TestSystemdNamespaceStartup$' -test.v
}
# Exact old failure -> preserve private namespace -> all proxy strategies ->
# separate Agent mount namespace -> core restart -> fresh and stale recovery.
run_fixture occupied
run_fixture legacy
run_fixture shadow
run_fixture reuse
for mode in fresh stale; do
  docker exec "$fixture_name" sh -ec 'systemctl stop incudal-xray.service; ip netns delete incudal-egress; nft delete table inet incudal_egress'
  run_fixture "$mode"
  run_fixture reuse
done
echo 'Real systemd/Xray namespace startup regressions passed.'
