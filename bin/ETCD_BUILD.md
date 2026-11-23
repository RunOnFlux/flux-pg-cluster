# Vendored etcd security build

These Linux/amd64 executables are built from upstream etcd **v3.5.35**, commit
`b0dc93710686bba178c9b6408e545be21d27b2ce`, with Go **1.27.4** and CGO disabled.
They are locally rebuilt executables, not the unmodified upstream release assets.
Go 1.27.4 also incorporates the standard-library fix for GO-2026-6859; this
toolchain-only refresh does not change etcd's protocol or persisted data format.

`etcd-security.patch` changes `golang.org/x/net` from v0.61.0 to **v0.62.0** in the
server and etcdctl module manifests/checksums to address **CVE-2026-78661**. It
does not change the etcd server code, protocol, or data format. Upstream
v3.5.35 bundles the affected dependency. The version output
uses Git SHA `b0dc937-flux-security` to identify this patched build.

The same patch upgrades OpenTelemetry core, SDK, metric, and trace packages to
**v1.47.0** for **CVE-2026-128085**. The OTLP exporters remain at v1.45.0
and gRPC instrumentation remains at v0.46.0 to preserve the interfaces used by
etcd 3.5.35. It also upgrades `golang.org/x/crypto` to **v0.59.0**,
clearing **GO-2026-5932** from binary/package audits. These dependency updates
do not change etcd storage or consensus.

Reproduce from the repository root:

```sh
bash scripts/rebuild-etcd.sh /tmp/flux-pg-etcd-rebuild
```

The source destination must be absent or empty. The script checks the upstream
commit, applies the checked-in patch, builds both binaries with a pinned
toolchain, and refreshes `SHA256SUMS`. The Dockerfile verifies those executable
hashes before installation. Validate scans, etcd startup, and existing-volume
rolling-upgrade behavior whenever changing these files.
