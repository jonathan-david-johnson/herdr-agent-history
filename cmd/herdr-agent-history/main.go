package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jonathan-david-johnson/herdr-agent-history/internal/herdr"
	"github.com/jonathan-david-johnson/herdr-agent-history/internal/tracker"
)

func defaultStateDir() string {
	if value := os.Getenv("HERDR_PLUGIN_STATE_DIR"); value != "" {
		return value
	}
	root := os.Getenv("XDG_STATE_HOME")
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(root, "herdr", "plugins", "local.agent-history")
}

func defaultSocket() string {
	if value := os.Getenv("HERDR_SOCKET_PATH"); value != "" {
		return value
	}
	root := os.Getenv("XDG_CONFIG_HOME")
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".config")
	}
	return filepath.Join(root, "herdr", "herdr.sock")
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: herdr-agent-history <track|previous|next|inspect|reset|version> [--socket PATH] [--state-dir PATH]")
	}
	action := args[0]
	if action == "version" {
		fmt.Println("herdr-agent-history 0.1.0")
		return nil
	}
	if action != "track" && action != "previous" && action != "next" && action != "inspect" && action != "reset" {
		return fmt.Errorf("unknown action %q", action)
	}
	flags := flag.NewFlagSet(action, flag.ContinueOnError)
	socket := flags.String("socket", defaultSocket(), "local Herdr API socket (one session per tracker)")
	stateDir := flags.String("state-dir", defaultStateDir(), "plugin state and private control socket directory")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if action == "track" {
		return tracker.Run(ctx, herdr.Client{SocketPath: *socket, Timeout: 3 * time.Second}, *socket, *stateDir)
	}
	ctx, timeoutCancel := context.WithTimeout(ctx, 8*time.Second)
	defer timeoutCancel()
	result, err := tracker.Invoke(ctx, *stateDir, tracker.Request{Action: action, HerdrSocket: *socket})
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "agent-history:", err)
		os.Exit(1)
	}
}
