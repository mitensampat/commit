package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/msfoundry/commit/extraction"
	"github.com/msfoundry/commit/server"
	"github.com/msfoundry/commit/store"
	"github.com/msfoundry/commit/whatsapp"
)

const defaultPort = 9384

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	dataDir, err := dataDirectory()
	if err != nil {
		log.Fatalf("failed to determine data directory: %v", err)
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		log.Fatalf("failed to create data directory: %v", err)
	}

	db, err := store.Open(filepath.Join(dataDir, "commit.db"))
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	extractor := extraction.New(db)
	wa := whatsapp.New(db, dataDir, extractor, ctx)
	srv := server.New(db, wa, defaultPort)

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("shutting down...")
		cancel()
	}()

	if db.GetAPIKey() != "" && wa.HasSession() {
		go wa.Connect(ctx)
	}

	addr := fmt.Sprintf("0.0.0.0:%d", defaultPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", addr, err)
	}
	log.Printf("Commit running at http://%s", addr)

	if err := srv.Serve(ctx, ln); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func dataDirectory() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	newDir := filepath.Join(home, ".owe")
	oldDir := filepath.Join(home, ".commit")
	if _, err := os.Stat(newDir); os.IsNotExist(err) {
		if _, err := os.Stat(oldDir); err == nil {
			os.Rename(oldDir, newDir)
		}
	}
	return newDir, nil
}
