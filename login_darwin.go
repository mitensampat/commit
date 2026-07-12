package main

import (
	"fmt"
	"os"
	"path/filepath"
)

const launchAgentLabel = "com.msfoundry.commit"

func launchAgentPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
}

func loginItemEnabled() bool {
	_, err := os.Stat(launchAgentPath())
	return err == nil
}

func enableLoginItem() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
</dict>
</plist>
`, launchAgentLabel, exe)
	if err := os.MkdirAll(filepath.Dir(launchAgentPath()), 0755); err != nil {
		return err
	}
	return os.WriteFile(launchAgentPath(), []byte(plist), 0644)
}

func disableLoginItem() error {
	err := os.Remove(launchAgentPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
