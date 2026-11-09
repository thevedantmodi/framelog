// Command framelog-sim runs the real Framelog pipeline end to end against a
// fake camera card, with no SD card, card reader, or photos needed. It walks
// through a series of shoot-day scenarios and prints what happens in each.
//
//	go run ./cmd/framelog-sim            # all scenarios, temp sandbox
//	go run ./cmd/framelog-sim -v         # also show framelog.log as it's written
//	go run ./cmd/framelog-sim -real-rclone
//	go run ./cmd/framelog-sim -only backup-full
//	go run ./cmd/framelog-sim -dir /tmp/fl -keep
//
// Exit status is 0 when every scenario passed. The sandbox (library, card
// receipts, catalog.db, framelog.log) is kept when a scenario fails or with
// -keep, so you can open it and look around.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/thevedantmodi/framelog/core/sim"
)

func main() {
	dir := flag.String("dir", "", "sandbox directory (default: a new temp dir)")
	keep := flag.Bool("keep", false, "keep the sandbox even when everything passes")
	verbose := flag.Bool("v", false, "also print framelog.log lines as the pipeline writes them")
	realRclone := flag.Bool("real-rclone", false, "use the installed rclone instead of the fake one")
	only := flag.String("only", "", "comma-separated scenario IDs to run (earlier ones run first): "+strings.Join(sim.IDs(), ", "))
	flag.Parse()

	opts := sim.Options{Dir: *dir, Out: os.Stdout, RealRclone: *realRclone}
	if *verbose {
		opts.PipelineLog = os.Stdout
	}
	if *only != "" {
		opts.Only = strings.Split(*only, ",")
	}

	fmt.Println("Framelog simulator — the real pipeline, a fake α7C II card")
	fmt.Println()
	sum, err := sim.Run(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "framelog-sim: %v\n", err)
		os.Exit(2)
	}

	passed, skipped := 0, 0
	for _, r := range sum.Results {
		switch {
		case r.Skipped:
			skipped++
		case r.Passed:
			passed++
		}
	}
	ran := len(sum.Results) - skipped
	fmt.Printf("%d of %d scenarios passed", passed, ran)
	if skipped > 0 {
		fmt.Printf(" (%d skipped)", skipped)
	}
	fmt.Println()

	if sum.Passed() && !*keep && *dir == "" {
		os.RemoveAll(sum.Dir)
		return
	}
	fmt.Printf("\nSandbox kept for inspection: %s\n", sum.Dir)
	fmt.Printf("  card receipts:  %s\n", sum.ReportsDir)
	fmt.Printf("  pipeline log:   %s\n", sum.LogFile)
	if !sum.Passed() {
		os.Exit(1)
	}
}
