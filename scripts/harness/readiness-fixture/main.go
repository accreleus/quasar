// Command readiness-fixture is the RH-02 (#264) readiness fault-injection
// harness's fixture. Test-only — see README.md for the "never reaches
// production" proof and how the harness drives it.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "relay":
		err = runRelayCmd(os.Args[2:])
	case "host":
		err = runHostCmd(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "readiness-fixture:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: readiness-fixture relay --listen ADDR --upstream WS_URL --control ADDR")
	fmt.Fprintln(os.Stderr, "       readiness-fixture host --control-plane WS_URL --node-name NAME --enrollment-token TOKEN [--slots N] [--vram-mb N] [--readiness-file PATH] [--report-interval DURATION]")
}

func runRelayCmd(args []string) error {
	fs := flag.NewFlagSet("relay", flag.ExitOnError)
	listen := fs.String("listen", ":8500", "address to accept the real node agent's WebSocket on")
	upstream := fs.String("upstream", "", "the control plane's agent WebSocket URL, e.g. ws://host:8080/agent/ws")
	control := fs.String("control", "127.0.0.1:8501", "loopback HTTP control address (/healthz, /rule, /stats)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *upstream == "" {
		return fmt.Errorf("--upstream is required")
	}

	relay := NewRelay(*listen, *upstream, *control)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return RunRelay(ctx, relay)
}

func runHostCmd(args []string) error {
	fs := flag.NewFlagSet("host", flag.ExitOnError)
	controlPlane := fs.String("control-plane", "", "the control plane's agent WebSocket URL")
	nodeName := fs.String("node-name", "", "this scripted host's node_name")
	enrollmentToken := fs.String("enrollment-token", "", "enrollment token")
	slots := fs.Int("slots", 1, "encode_slots_total to report on the one GPU")
	vramMB := fs.Int("vram-mb", 8192, "vram_mb_total to report on the one GPU")
	readinessFile := fs.String("readiness-file", "", "path to a JSON array of readiness checks, re-read on every report")
	reportInterval := fs.Duration("report-interval", 10*time.Second, "capacity re-report cadence")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *controlPlane == "" || *nodeName == "" || *enrollmentToken == "" {
		return fmt.Errorf("--control-plane, --node-name and --enrollment-token are all required")
	}

	h := NewHost(HostConfig{
		ControlPlaneURL: *controlPlane,
		NodeName:        *nodeName,
		EnrollmentToken: *enrollmentToken,
		Slots:           *slots,
		VRAMMB:          *vramMB,
		ReadinessFile:   *readinessFile,
		ReportInterval:  *reportInterval,
	})

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return h.Run(ctx)
}
