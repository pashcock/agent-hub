package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/pashcock/agent-hub/internal/config"
)

func providerControl(cfg *config.Config, action string, args []string) error {
	fs := flag.NewFlagSet(action, flag.ContinueOnError)
	reason := fs.String("reason", "", "operator reason")
	duration := fs.Duration("for", 0, "pause duration, e.g. 5h; omit for indefinite pause")
	// Provider is first, flags follow it, unlike flag's default positional handling.
	if len(args) < 1 {
		return fmt.Errorf("usage: agent-hub %s <agent> [--reason text] [--for 5h]", action)
	}
	agent := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *duration < 0 {
		return fmt.Errorf("invalid arguments")
	}
	if action == "resume" && (*duration != 0 || *reason != "") {
		return fmt.Errorf("resume takes only the agent name")
	}
	payload := map[string]any{"agent": agent, "action": action, "reason": *reason}
	if *duration > 0 {
		payload["until"] = time.Now().Add(*duration)
	}
	body, _ := json.Marshal(payload)
	dir, err := config.StateDir()
	if err != nil {
		return err
	}
	token, err := os.ReadFile(filepath.Join(dir, "token"))
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+cfg.Server.Addr+"/operator/provider", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+string(bytes.TrimSpace(token)))
	client := http.Client{Timeout: 2 * time.Minute}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 8192))
		return fmt.Errorf("daemon returned %s: %s", res.Status, b)
	}
	fmt.Printf("%s: %s applied\n", agent, action)
	return nil
}
