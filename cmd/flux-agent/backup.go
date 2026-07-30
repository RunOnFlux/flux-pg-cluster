package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/RunOnFlux/flux-pg-cluster/internal/config"
	pkglog "github.com/RunOnFlux/flux-pg-cluster/internal/log"
	"github.com/RunOnFlux/flux-pg-cluster/internal/patroni"
)

// backupFilePrefix and backupFileSuffix bracket the names this agent creates and
// is allowed to prune. We only ever delete files matching this pattern so we
// never touch unrelated files a user may have placed in BACKUP_DIR.
const (
	backupFilePrefix  = "pgdumpall-"
	backupFileSuffix  = ".sql.gz"
	dumpCompleteMark  = "PostgreSQL database dump complete"
	minPlausibleBytes = 100
)

// runBackup is a long-running loop that takes periodic logical backups
// (pg_dumpall) of the cluster, but only when this node is a healthy, running
// primary. Key safety properties:
//
//   - Backups run only on the primary, so we keep a single authoritative copy
//     rather than one per node.
//   - A backup is written to a temp file and integrity-checked (gzip is valid
//     and the pg_dumpall completion marker is present) before it replaces
//     anything. If the database/cluster is unhealthy or the dump is truncated,
//     we keep the previous good backups untouched — we never overwrite a good
//     backup with a broken one.
//   - Retention is enforced after each successful backup: keep the newest
//     BACKUP_RETENTION_COUNT files, then drop oldest ones until the total size
//     is under BACKUP_MAX_TOTAL_BYTES (0 = unlimited). The just-made backup is
//     never pruned.
func runBackup(args []string) {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	_ = fs.Parse(args)

	pkglog.Section("BACKUP AGENT STARTING (Go agent)")

	if !envBoolOr("BACKUP_ENABLED", false) {
		pkglog.Infof("BACKUP_ENABLED is not true — backup agent idle (set BACKUP_ENABLED=true to enable)")
		// Block forever so supervisord treats us as running rather than
		// crash-looping a process that immediately exits.
		for {
			time.Sleep(24 * time.Hour)
		}
	}

	cfg := config.FromEnv()
	if err := config.LoadClusterEnv(cfg); err != nil {
		pkglog.Warnf("could not load %s: %v — continuing with env-derived values", config.ClusterEnvFile, err)
	}

	dir := envStrOr("BACKUP_DIR", "/var/lib/postgresql/backups")
	interval := envIntOr("BACKUP_INTERVAL_SECONDS", 86400)
	if interval < 60 {
		pkglog.Warnf("BACKUP_INTERVAL_SECONDS too low (%d) — clamping to 60", interval)
		interval = 60
	}
	retain := envIntOr("BACKUP_RETENTION_COUNT", 1)
	if retain < 1 {
		retain = 1
	}
	maxBytes := envInt64Or("BACKUP_MAX_TOTAL_BYTES", 0) // 0 = unlimited

	pkglog.Infof("backup config: dir=%s interval=%ds retention=%d max_total_bytes=%d",
		dir, interval, retain, maxBytes)

	for {
		runBackupCycle(cfg, dir, retain, maxBytes)
		pkglog.Infof("next backup attempt in %ds", interval)
		time.Sleep(time.Duration(interval) * time.Second)
	}
}

func runBackupCycle(cfg *config.Config, dir string, retain int, maxBytes int64) {
	pkglog.Section(fmt.Sprintf("BACKUP CYCLE - %s", time.Now().Format(time.RFC3339)))

	// Gate 1: only a healthy, running primary backs up. This both avoids N
	// copies (one per node) and prevents dumping a lagging or initializing
	// replica or a cluster with no healthy leader.
	if !localIsHealthyPrimary(cfg) {
		pkglog.Infof("skipping backup: this node is not a healthy, running primary")
		return
	}

	pgDumpAll, err := discoverPgDumpAll()
	if err != nil {
		pkglog.Errorf("skipping backup: %v", err)
		return
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		pkglog.Errorf("skipping backup: cannot create backup dir %s: %v", dir, err)
		return
	}

	ts := time.Now().UTC().Format("20060102T150405Z")
	finalPath := filepath.Join(dir, backupFilePrefix+ts+backupFileSuffix)
	tmpPath := finalPath + ".tmp"

	// Gate 2: dump to a temp file. If anything fails, the previous good
	// backups stay untouched.
	if err := writeDump(cfg, pgDumpAll, tmpPath); err != nil {
		pkglog.Errorf("backup FAILED — keeping previous backups intact: %v", err)
		_ = os.Remove(tmpPath)
		return
	}

	// Gate 3: integrity check before we trust this file.
	if err := verifyDump(tmpPath); err != nil {
		pkglog.Errorf("backup integrity check FAILED — discarding, keeping previous backups: %v", err)
		_ = os.Remove(tmpPath)
		return
	}

	// Atomically promote the verified temp file to its final name.
	if err := os.Rename(tmpPath, finalPath); err != nil {
		pkglog.Errorf("could not finalize backup: %v", err)
		_ = os.Remove(tmpPath)
		return
	}
	if fi, err := os.Stat(finalPath); err == nil {
		pkglog.Infof("backup OK: %s (%s)", finalPath, humanBytes(fi.Size()))
	}

	// Only now do we prune — never before we have a fresh, verified backup.
	pruneBackups(dir, retain, maxBytes, finalPath)
}

// localIsHealthyPrimary returns true only if the local Patroni reports this node
// as a running primary. We query the local REST API on 127.0.0.1 to avoid
// hairpin-NAT issues with the node's own public IP.
func localIsHealthyPrimary(cfg *config.Config) bool {
	pc := patroni.New(cfg.SSLEnabled, cfg.PatroniAPIPort)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := pc.GetInfo(ctx, "127.0.0.1")
	if err != nil || info == nil {
		pkglog.Infof("local Patroni not reachable/healthy: %v", err)
		return false
	}
	roleOK := info.Role == "primary" || info.Role == "master"
	stateOK := info.State == "running"
	if !roleOK || !stateOK {
		pkglog.Infof("local node role=%q state=%q — not a backup target", info.Role, info.State)
		return false
	}
	return true
}

// writeDump runs pg_dumpall and streams its (gzip-compressed) output to tmpPath.
// It connects over the local unix socket as the postgres OS user (peer auth);
// PGPASSWORD is provided as a fallback for md5/TCP setups.
func writeDump(cfg *config.Config, pgDumpAll, tmpPath string) error {
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer f.Close()

	gz := gzip.NewWriter(f)

	args := []string{
		"-h", "/var/run/postgresql",
		"-p", strconv.Itoa(cfg.PostgresPort),
		"-U", "postgres",
		"--clean", "--if-exists",
	}
	cmd := exec.Command(pgDumpAll, args...)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+cfg.PostgresSuperuserPassword)
	cmd.Stdout = gz
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	// Always flush/close the gzip writer before evaluating the result.
	if cerr := gz.Close(); cerr != nil && runErr == nil {
		runErr = cerr
	}
	if syncErr := f.Sync(); syncErr != nil && runErr == nil {
		runErr = syncErr
	}
	if runErr != nil {
		return fmt.Errorf("pg_dumpall: %v (stderr: %s)", runErr, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// verifyDump confirms the temp backup is a valid gzip stream that contains the
// pg_dumpall completion marker, proving the dump finished rather than being cut
// off mid-stream.
func verifyDump(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Size() < minPlausibleBytes {
		return fmt.Errorf("dump file is implausibly small (%d bytes)", fi.Size())
	}
	ok, err := gzipContains(path, dumpCompleteMark)
	if err != nil {
		return fmt.Errorf("reading dump: %w", err)
	}
	if !ok {
		return fmt.Errorf("completion marker %q not found — dump likely truncated", dumpCompleteMark)
	}
	return nil
}

// gzipContains streams a gzip file and reports whether the (decompressed)
// content contains needle. It searches in chunks, carrying over a small tail so
// a match spanning a chunk boundary is still found.
func gzipContains(path, needle string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return false, err
	}
	defer gz.Close()

	nb := []byte(needle)
	buf := make([]byte, 64*1024)
	var carry []byte
	for {
		n, rerr := gz.Read(buf)
		if n > 0 {
			hay := append(carry, buf[:n]...)
			if bytes.Contains(hay, nb) {
				return true, nil
			}
			if keep := len(nb) - 1; keep > 0 && len(hay) > keep {
				carry = append(carry[:0], hay[len(hay)-keep:]...)
			} else {
				carry = append(carry[:0], hay...)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return false, rerr
		}
	}
	return false, nil
}

type backupFile struct {
	path string
	mod  time.Time
	size int64
}

// pruneBackups enforces retention: keep the newest `retain` files, then drop
// oldest files until the total size is under maxBytes (0 = unlimited). keepPath
// (the just-made backup) is never deleted.
func pruneBackups(dir string, retain int, maxBytes int64, keepPath string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		pkglog.Warnf("prune: read dir: %v", err)
		return
	}
	var files []backupFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, backupFilePrefix) || !strings.HasSuffix(name, backupFileSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, backupFile{filepath.Join(dir, name), info.ModTime(), info.Size()})
	}
	// newest first
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })

	// 1) Retention count.
	var kept []backupFile
	for i, fbk := range files {
		if i < retain || fbk.path == keepPath {
			kept = append(kept, fbk)
			continue
		}
		if err := os.Remove(fbk.path); err != nil {
			pkglog.Warnf("prune: remove %s: %v", fbk.path, err)
			kept = append(kept, fbk)
		} else {
			pkglog.Infof("pruned old backup (retention): %s", filepath.Base(fbk.path))
		}
	}

	// 2) Size cap.
	if maxBytes <= 0 {
		return
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].mod.After(kept[j].mod) })
	var total int64
	for _, fbk := range kept {
		total += fbk.size
	}
	for len(kept) > 1 && total > maxBytes {
		oldest := kept[len(kept)-1]
		if oldest.path == keepPath {
			break // never delete the backup we just made
		}
		if err := os.Remove(oldest.path); err != nil {
			pkglog.Warnf("prune: remove %s: %v", oldest.path, err)
			break
		}
		pkglog.Infof("pruned old backup (size cap): %s", filepath.Base(oldest.path))
		total -= oldest.size
		kept = kept[:len(kept)-1]
	}
	if total > maxBytes {
		pkglog.Warnf("remaining backups (%s) still exceed BACKUP_MAX_TOTAL_BYTES (%s) — keeping the latest anyway; raise the cap if this is unexpected",
			humanBytes(total), humanBytes(maxBytes))
	}
}

// discoverPgDumpAll locates pg_dumpall in the same bin dir as the running PG.
func discoverPgDumpAll() (string, error) {
	dir, err := discoverPostgresBinDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "pg_dumpall")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("pg_dumpall not found at %s: %w", p, err)
	}
	return p, nil
}

func envBoolOr(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	return v == "true" || v == "1" || v == "yes" || v == "on"
}

func envStrOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt64Or(key string, def int64) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
