// Command flux-agent is the all-in-one cluster management agent that replaces
// the entrypoint.sh, start-etcd.sh, update-cluster.sh shell scripts and adds a
// TCP proxy that routes writes to the current Patroni primary.
package main

import (
	"fmt"
	"os"

	pkglog "github.com/RunOnFlux/flux-pg-cluster/internal/log"
)

var version = "dev"

func usage() {
	fmt.Fprintf(os.Stderr, `flux-agent — Flux PostgreSQL cluster agent (version %s)

Usage:
  flux-agent <subcommand> [args]

Subcommands:
  init         Run one-shot cluster initialization (replaces entrypoint.sh)
  etcd-start   Start etcd with join/bootstrap logic (replaces start-etcd.sh)
  daemon       Run the cluster reconciliation loop (replaces update-cluster.sh)
  proxy        Run TCP proxy routing writes to the current Patroni primary
  version      Print version and exit
  help         Print this help message
`, version)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "init":
		runInit(os.Args[2:])
	case "etcd-start":
		runEtcdStart(os.Args[2:])
	case "daemon":
		runDaemon(os.Args[2:])
	case "proxy":
		runProxy(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		usage()
	default:
		pkglog.Errorf("unknown subcommand: %q", os.Args[1])
		usage()
		os.Exit(2)
	}
}
