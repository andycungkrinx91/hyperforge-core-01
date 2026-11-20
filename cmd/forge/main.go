package main

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"syscall"

	"hyperforge/internal/forge"
)

func main() {
	runtime.GOMAXPROCS(2)
	debug.SetGCPercent(1000)
	debug.SetMemoryLimit(768 << 20)

	login := os.Getenv("MAKEFUN_FORGE_LOGIN")
	if login == "" {
		login = "adaiversen"
	}
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	trialNow := os.Getenv("MAKEFUN_TRIAL_NOW")
	if trialNow == "" {
		trialNow = "2026-11-14T00:00:00Z"
	}
	bindPort := 0
	if p := os.Getenv("MAKEFUN_FORGE_PORT"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v >= 0 && v <= 65535 {
			bindPort = v
		}
	}

	store := forge.NewStore(login, token, trialNow)
	srv, err := forge.NewEpollServer(store, bindPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen failed: %v\n", err)
		os.Exit(1)
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		srv.Close()
		os.Exit(0)
	}()

	fmt.Fprintf(os.Stdout, "PORT=%d\n", srv.Port())
	if err := srv.Serve(); err != nil {
		fmt.Fprintf(os.Stderr, "[forge] serve error: %v\n", err)
		os.Exit(1)
	}
}
