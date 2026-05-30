package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/msfoundry/commit/extraction"
	"github.com/msfoundry/commit/server"
	"github.com/msfoundry/commit/store"
	"github.com/msfoundry/commit/whatsapp"

	waStore "go.mau.fi/whatsmeow/store"
)

const defaultPort = 9384

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	waStore.SetOSInfo("Commit", [3]uint32{1, 1, 1})

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

	extractor := extraction.New(db, nil)
	wa := whatsapp.New(db, dataDir, extractor, ctx)
	extractor.SetNotifier(wa)
	srv := server.New(db, wa, extractor, defaultPort)

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

	hasHostsEntry := ensureHostsEntry()

	addr := fmt.Sprintf("0.0.0.0:%d", defaultPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", addr, err)
	}

	if hasHostsEntry {
		log.Printf("Commit running at http://commit:%d", defaultPort)
		openBrowser(fmt.Sprintf("http://commit:%d", defaultPort))
	} else {
		log.Printf("Commit running at http://localhost:%d", defaultPort)
		openBrowser(fmt.Sprintf("http://localhost:%d", defaultPort))
	}

	if err := srv.Serve(ctx, ln); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func ensureHostsEntry() bool {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return false
	}
	data, err := os.ReadFile("/etc/hosts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		for _, f := range fields[1:] {
			if f == "commit" {
				return true
			}
		}
	}
	log.Println("Adding 'commit' to /etc/hosts (one-time setup, requires admin password)...")
	script := `do shell script "echo '127.0.0.1 commit' >> /etc/hosts" with administrator privileges`
	cmd := exec.Command("osascript", "-e", script)
	if err := cmd.Run(); err != nil {
		log.Printf("Could not add hosts entry: %v (you can still use localhost:%d)", err, defaultPort)
		return false
	}
	log.Println("Added 'commit' to /etc/hosts — use http://commit:" + fmt.Sprint(defaultPort))
	return true
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("Could not open browser: %v — open %s manually", err, url)
	}
}

func dataDirectory() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	newDir := filepath.Join(home, ".commit")
	oldDir := filepath.Join(home, ".owe")
	if _, err := os.Stat(newDir); os.IsNotExist(err) {
		if _, err := os.Stat(oldDir); err == nil {
			os.Rename(oldDir, newDir)
		}
	}
	return newDir, nil
}
