package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/evanxiao/quickworks/internal/app"
)

func main() {
	if len(os.Args) < 2 {
		fatal(errors.New("usage: quickworks <server|provisioner|agent> --config PATH"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if os.Args[1] == "provisioner" && len(os.Args) > 2 && os.Args[2] == "restore" {
		restore(ctx, os.Args[3:])
		return
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "path to YAML configuration")
	_ = fs.Parse(os.Args[2:])
	var err error
	switch os.Args[1] {
	case "server":
		err = app.RunServer(ctx, *configPath)
	case "provisioner":
		err = app.RunProvisioner(ctx, *configPath)
	case "agent":
		err = app.RunAgent(ctx)
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fatal(err)
	}
}

func restore(_ context.Context, args []string) {
	fs := flag.NewFlagSet("provisioner restore", flag.ExitOnError)
	configPath := fs.String("config", "config.provisioner.yaml", "path to provisioner YAML configuration")
	workspaceID := fs.String("workspace", "", "workspace ID")
	snapshot := fs.String("snapshot", "", "retained .tfstate snapshot filename")
	_ = fs.Parse(args)
	if *workspaceID == "" || *snapshot == "" {
		fatal(errors.New("usage: quickworks provisioner restore --config PATH --workspace ID --snapshot FILE"))
	}
	if err := app.RestoreLocalState(*configPath, *workspaceID, *snapshot); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "quickworks:", err)
	os.Exit(1)
}
