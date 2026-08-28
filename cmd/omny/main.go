package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/bharathajjarapu/omny/internal/edit"
	"github.com/bharathajjarapu/omny/internal/omny"
)

// Set by -ldflags. Empty uses the toolchain's VCS stamp.
var version string

func main() {
	path := flag.String("c", "omny.yaml", "config file")
	show := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() { edit.Usage(os.Stderr) }
	flag.Parse()

	if *show {
		fmt.Println("omny", stamp())
		return
	}

	if flag.NArg() > 0 {
		if err := edit.CLI(os.Stdout, os.Stdin, *path, flag.Args()); err != nil {
			fmt.Fprintln(os.Stderr, "omny:", err)
			os.Exit(1)
		}
		return
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	ctx, quit := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer quit()
	if err := omny.Run(ctx, *path); err != nil {
		fmt.Fprintln(os.Stderr, "omny:", err)
		os.Exit(1)
	}
}

func stamp() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return "devel"
}
