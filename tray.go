package main

import (
	_ "embed"
	"context"
	"fmt"
	"log"
	"runtime"
	"time"

	"fyne.io/systray"

	"github.com/msfoundry/commit/extraction"
	"github.com/msfoundry/commit/store"
	"github.com/msfoundry/commit/whatsapp"
)

//go:embed assets/tray.png
var trayIconPNG []byte

//go:embed assets/tray_dim.png
var trayIconDimPNG []byte

//go:embed assets/tray_attn.png
var trayIconAttnPNG []byte

//go:embed assets/tray.ico
var trayIconICO []byte

//go:embed assets/tray_attn.ico
var trayIconAttnICO []byte

const (
	trayModelFast  = store.FallbackModel // Haiku
	trayModelSharp = store.DefaultModel  // Sonnet
)

type trayState int

const (
	stateConnected trayState = iota
	stateDisconnected
	stateAttention
)

// runTray owns the process main thread (required by AppKit). The server and
// WhatsApp loops run in goroutines started before this is called.
func runTray(ctx context.Context, cancel context.CancelFunc, db *store.DB, wa *whatsapp.Client, ext *extraction.Extractor, dashboardURL string) {
	onReady := func() {
		setTrayIcon(stateConnected)
		systray.SetTooltip("Commit")

		status := systray.AddMenuItem("Starting...", "")
		status.Disable()
		stats := systray.AddMenuItem("", "")
		stats.Disable()
		systray.AddSeparator()

		openDash := systray.AddMenuItem("Open Dashboard", "Open Commit in your browser")
		systray.AddSeparator()

		model := systray.AddMenuItem("Model", "Claude model used for extraction")
		modelFast := model.AddSubMenuItemCheckbox("Claude Haiku — fast", "", false)
		modelSharp := model.AddSubMenuItemCheckbox("Claude Sonnet — sharper", "", false)
		syncModelChecks := func() {
			if db.GetModel() == trayModelSharp {
				modelFast.Uncheck()
				modelSharp.Check()
			} else {
				modelFast.Check()
				modelSharp.Uncheck()
			}
		}
		syncModelChecks()

		login := systray.AddMenuItemCheckbox("Start at Login", "Launch Commit when you log in", loginItemEnabled())
		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit Commit", "")

		refresh := func() {
			state := stateConnected
			statusText := "Connected to WhatsApp"
			if !wa.IsConnected() {
				state = stateDisconnected
				statusText = "WhatsApp disconnected"
			}
			// extraction failing recently outranks the connection state
			if ds := ext.GetDebugStatus(); ds.LastErrorAt != "" {
				if t, err := time.Parse(time.RFC3339, ds.LastErrorAt); err == nil && time.Since(t) < 10*time.Minute {
					state = stateAttention
					statusText = "Extraction failing — open dashboard"
				}
			}
			setTrayIcon(state)
			status.SetTitle(statusText)

			today, err1 := db.GetDayStats(0)
			yday, err2 := db.GetDayStats(1)
			if err1 == nil && err2 == nil {
				stats.SetTitle(fmt.Sprintf("Yesterday %d msgs · %d found — today %d · %d",
					yday.Messages, yday.Commitments, today.Messages, today.Commitments))
			}
		}
		refresh()

		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					refresh()
				case <-openDash.ClickedCh:
					openBrowser(dashboardURL)
				case <-modelFast.ClickedCh:
					if err := db.SetModel(trayModelFast); err != nil {
						log.Printf("set model error: %v", err)
					}
					syncModelChecks()
				case <-modelSharp.ClickedCh:
					if err := db.SetModel(trayModelSharp); err != nil {
						log.Printf("set model error: %v", err)
					}
					syncModelChecks()
				case <-login.ClickedCh:
					if login.Checked() {
						if err := disableLoginItem(); err != nil {
							log.Printf("disable login item: %v", err)
						} else {
							login.Uncheck()
						}
					} else {
						if err := enableLoginItem(); err != nil {
							log.Printf("enable login item: %v", err)
						} else {
							login.Check()
						}
					}
				case <-quit.ClickedCh:
					systray.Quit()
				}
			}
		}()
	}

	onExit := func() {
		log.Println("tray exit — shutting down")
		cancel()
		// give the HTTP server a moment to drain
		time.Sleep(300 * time.Millisecond)
	}

	systray.Run(onReady, onExit)
}

// quitTray tears down the systray loop; safe to call more than once.
func quitTray() {
	defer func() { recover() }()
	systray.Quit()
}

func setTrayIcon(s trayState) {
	if runtime.GOOS == "windows" {
		switch s {
		case stateAttention:
			systray.SetIcon(trayIconAttnICO)
		default:
			systray.SetIcon(trayIconICO)
		}
		return
	}
	switch s {
	case stateConnected:
		systray.SetTemplateIcon(trayIconPNG, trayIconPNG)
	case stateDisconnected:
		systray.SetTemplateIcon(trayIconDimPNG, trayIconDimPNG)
	case stateAttention:
		// colored dot — deliberately not a template image
		systray.SetIcon(trayIconAttnPNG)
	}
}
