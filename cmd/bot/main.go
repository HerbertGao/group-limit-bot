package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/herbertgao/group-limit-bot/internal/runtime"
	"github.com/herbertgao/group-limit-bot/internal/updater"
	"github.com/herbertgao/group-limit-bot/internal/version"
)

const usage = `group-limit-bot — Telegram channel-gated guardian bot

Usage:
  group-limit-bot [--config PATH]     run the bot (default)
  group-limit-bot update              check GitHub for a newer release and
                                      replace the current binary in-place
  group-limit-bot version             print version/build info and exit
  group-limit-bot --version           same as above
  group-limit-bot --help              show this message

After an update, restart the process (e.g. 'systemctl restart group-limit-bot')
for the new binary to take effect.`

func main() {
	// Subcommand dispatch before flag parsing so `update` / `version` don't
	// get mistaken for config-path typos.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "update":
			if err := updater.NewUpdater().Update(); err != nil {
				fmt.Fprintf(os.Stderr, "update failed: %v\n", err)
				os.Exit(1)
			}
			return
		case "version":
			fmt.Println(version.GetFullVersionInfo())
			return
		case "help", "-h", "--help":
			fmt.Println(usage)
			return
		}
	}

	var (
		configPath  string
		showVersion bool
	)
	flag.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	flag.StringVar(&configPath, "config", "./config.yaml", "path to config.yaml")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Println(version.GetFullVersionInfo())
		return
	}

	ctx := context.Background()
	if err := runtime.Run(ctx, configPath); err != nil {
		fmt.Fprintln(os.Stderr, "FATAL:", err)
		log.Println("FATAL:", err)
		os.Exit(1)
	}
}
