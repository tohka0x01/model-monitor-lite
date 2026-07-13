package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerUpdatePreservesConfigurationAndRefreshesImage(t *testing.T) {
	installDir := filepath.Join(t.TempDir(), "install")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatalf("create install directory: %v", err)
	}
	dataDir := filepath.Join(installDir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("create existing data directory: %v", err)
	}
	historyContent := []byte("existing-history")
	if err := os.WriteFile(filepath.Join(dataDir, "model-monitor.db"), historyContent, 0o600); err != nil {
		t.Fatalf("write existing history: %v", err)
	}
	envContent := "SQL_DSN=existing-value\nSERVER_PORT=9911\nBASE_PATH=/monitor\n"
	if err := os.WriteFile(filepath.Join(installDir, ".env"), []byte(envContent), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	composeContent := "services:\n  model-monitor-lite:\n    image: ghcr.io/tohka0x01/model-monitor-lite:latest\n"
	if err := os.WriteFile(filepath.Join(installDir, "docker-compose.yml"), []byte(composeContent), 0o644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}

	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatalf("create fake bin: %v", err)
	}
	commandLog := filepath.Join(t.TempDir(), "commands.log")
	writeExecutable(t, filepath.Join(fakeBin, "docker"), `#!/usr/bin/env bash
printf 'docker %s\n' "$*" >> "$COMMAND_LOG"
exit 0
`)
	writeExecutable(t, filepath.Join(fakeBin, "curl"), `#!/usr/bin/env bash
printf 'curl %s\n' "$*" >> "$COMMAND_LOG"
exit 0
`)

	cmd := exec.Command("bash", "install-linux.sh", "--update")
	cmd.Env = append(os.Environ(),
		"INSTALL_DIR="+filepath.ToSlash(installDir),
		"COMMAND_LOG="+filepath.ToSlash(commandLog),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("install-linux.sh --update error = %v\n%s", err, output.String())
	}

	gotEnv, err := os.ReadFile(filepath.Join(installDir, ".env"))
	if err != nil {
		t.Fatalf("read preserved env file: %v", err)
	}
	if string(gotEnv) != envContent {
		t.Fatalf(".env changed during update:\n%s", gotEnv)
	}
	commands, err := os.ReadFile(commandLog)
	if err != nil {
		t.Fatalf("read command log: %v", err)
	}
	commandText := string(commands)
	if !strings.Contains(commandText, "compose --env-file .env pull") {
		t.Fatalf("commands missing image pull:\n%s", commandText)
	}
	if !strings.Contains(commandText, "compose --env-file .env up -d --force-recreate") {
		t.Fatalf("commands missing forced recreation:\n%s", commandText)
	}
	if _, err := os.Stat(filepath.Join(installDir, "data")); err != nil {
		t.Fatalf("persistent data directory was not created: %v", err)
	}
	gotHistory, err := os.ReadFile(filepath.Join(dataDir, "model-monitor.db"))
	if err != nil {
		t.Fatalf("read preserved history: %v", err)
	}
	if !bytes.Equal(gotHistory, historyContent) {
		t.Fatalf("history data changed during update: %q", gotHistory)
	}
	if _, err := os.Stat(filepath.Join(installDir, "docker-compose.override.yml")); err != nil {
		t.Fatalf("history volume override was not created: %v", err)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}
