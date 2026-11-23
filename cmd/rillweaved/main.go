package main

import (
	"context"
	"encoding/json"
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

	"rillweave/disk"
	"rillweave/engine"
	"rillweave/wire"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:8174", "HTTP listen address")
	root := flag.String("data", "rillweave-data", "directory holding the active graph storage")
	migrate := flag.Bool("migrate", false, "convert a legacy per-graph .rill data directory to the SQLite database and exit")
	flag.Parse()
	if *migrate {
		if err := runMigration(*root); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := run(*addr, *root); err != nil {
		log.Fatal(err)
	}
}

// runMigration performs the offline storage cutover. It never serves traffic;
// the exclusive data-directory lock makes a concurrent server fail instead of
// mixing two storage modes.
func runMigration(root string) error {
	report, err := disk.Migrate(root)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	if report.AlreadyMigrated {
		log.Printf("data directory %q already uses SQLite storage; verification and cleanup completed", root)
	} else {
		log.Printf("data directory %q migrated to SQLite storage with %d graph head(s)", root, len(report.Heads))
	}
	return nil
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
