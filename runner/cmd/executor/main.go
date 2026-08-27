package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"syscall"

	"github.com/hkjang/releasedock/runner/internal/executor"
)

var (
	version = "dev"
	commit  = "unknown"
	builtAt = "unknown"
)

func main() {
	showVersion := flag.Bool("version", false, "print executor version")
	flag.Parse()
	if *showVersion {
		fmt.Printf("releasedock-executor %s (%s, %s)\n", version, commit, builtAt)
		return
	}
	if err := run(); err != nil {
		log.Printf("releasedock executor stopped: %v", err)
		os.Exit(1)
	}
}

func run() error {
	if os.Geteuid() == 0 {
		return errors.New("releasedock-executor refuses to run as root")
	}
	runnerUID, err := lookupSystemUID("releasedock-runner")
	if err != nil {
		return fmt.Errorf("resolve Runner account: %w", err)
	}
	if uint32(os.Geteuid()) == runnerUID {
		return errors.New("executor and Runner must use different operating-system users")
	}
	// systemd creates a fresh private /run directory for each one-request
	// activation. Validate it and sweep any SIGKILL remnant before accepting a
	// file descriptor from Runner.
	if err := executor.PrepareExecutorCredentialRoot(executor.DefaultExecutorCredentialRoot); err != nil {
		return err
	}
	listener, err := activatedListener(3)
	if err != nil {
		return err
	}
	defer listener.Close()
	server, err := executor.NewServer(executor.DefaultWorkspaceRoot, runnerUID, executor.Runner{})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("releasedock-executor %s listening on %s", version, executor.DefaultSocketPath)
	return server.Serve(ctx, listener)
}

// systemd socket activation passes the root-owned listening socket as fd 3.
// Reading the descriptor directly avoids adding another runtime configuration
// variable and prevents the unprivileged executor UID from replacing the socket.
func activatedListener(fd uintptr) (*net.UnixListener, error) {
	file := os.NewFile(fd, "releasedock-executor-listener")
	if file == nil {
		return nil, errors.New("systemd executor socket descriptor is unavailable")
	}
	listener, err := net.FileListener(file)
	_ = file.Close()
	if err != nil {
		return nil, fmt.Errorf("open systemd executor socket: %w", err)
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		_ = listener.Close()
		return nil, errors.New("systemd executor socket is not Unix domain socket")
	}
	if unixListener.Addr().String() != executor.DefaultSocketPath {
		_ = unixListener.Close()
		return nil, fmt.Errorf("executor socket must be %s", executor.DefaultSocketPath)
	}
	return unixListener, nil
}

func lookupSystemUID(name string) (uint32, error) {
	account, err := user.Lookup(name)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse UID for %s: %w", name, err)
	}
	return uint32(value), nil
}
