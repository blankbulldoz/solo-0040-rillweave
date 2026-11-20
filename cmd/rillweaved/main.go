package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"rillweave/engine"
	"rillweave/wire"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:8174", "HTTP listen address")
	root := flag.String("data", "rillweave-data", "directory of durable graph heads")
	flag.Parse()
	if err := run(*addr, *root); err != nil {
		log.Fatal(err)
	}
}

func run(addr, root string) error {
	db, err := engine.Open(root)
	if err != nil {
		return err
	}
	defer db.Close()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler: wire.New(db), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(listener)
	}()
	fmt.Printf("listening %s\n", listener.Addr())
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			// Exit without releasing the directory while a handler may still publish.
			log.Fatalf("shutdown: %v", err)
		}
		err := <-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
