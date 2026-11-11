#!/usr/bin/env bash
# Rebuild the vendored Linux/amd64 tools without changing the etcd release.
set -euo pipefail

repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
source_dir=${1:-$(mktemp -d -t flux-pg-etcd-build.XXXXXXXX)}
source_commit=b0dc93710686bba178c9b6408e545be21d27b2ce

git clone --depth 1 --branch v3.5.35 https://github.com/etcd-io/etcd.git "$source_dir"
test "$(git -C "$source_dir" rev-parse HEAD)" = "$source_commit"
git -C "$source_dir" apply --unidiff-zero "$repo_dir/bin/etcd-xnet.patch"

export GOTOOLCHAIN=go1.27.4 CGO_ENABLED=0 GOOS=linux GOARCH=amd64
for module in server etcdctl; do
  binary=etcdctl
  if [[ "$module" == server ]]; then binary=etcd; fi
  (
    cd "$source_dir/$module"
    go build -trimpath -buildvcs=false \
      -ldflags '-s -w -X go.etcd.io/etcd/api/v3/version.GitSHA=b0dc937-flux-xnet062' \
      -o "$repo_dir/bin/$binary" .
  )
done

(cd "$repo_dir/bin" && sha256sum etcd etcdctl > SHA256SUMS)
printf 'Built etcd 3.5.35 with x/net v0.62.0; retained source at %s\n' "$source_dir"
