package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/herbertgao/group-limit-bot/internal/runtime"
)

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", "./config.yaml", "path to config.yaml")
	flag.Parse()

	ctx := context.Background()
	if err := runtime.Run(ctx, configPath); err != nil {
		fmt.Fprintln(os.Stderr, "FATAL:", err)
		log.Println("FATAL:", err)
		os.Exit(1)
	}
}
