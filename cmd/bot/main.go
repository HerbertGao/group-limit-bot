package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/herbertgao/group-limit-bot/internal/runtime"
	"github.com/herbertgao/group-limit-bot/internal/version"
)

func main() {
	var (
		configPath  string
		showVersion bool
	)
	flag.StringVar(&configPath, "config", "./config.yaml", "path to config.yaml")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Println(version.Version)
		return
	}

	ctx := context.Background()
	if err := runtime.Run(ctx, configPath); err != nil {
		fmt.Fprintln(os.Stderr, "FATAL:", err)
		log.Println("FATAL:", err)
		os.Exit(1)
	}
}
