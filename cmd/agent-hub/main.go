// Command agent-hub is the local coordination daemon for native coding agents.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/claude"
	"github.com/pashcock/agent-hub/internal/codex"
	"github.com/pashcock/agent-hub/internal/config"
	"github.com/pashcock/agent-hub/internal/doctor"
	"github.com/pashcock/agent-hub/internal/grok"
	"github.com/pashcock/agent-hub/internal/hub"
	"github.com/pashcock/agent-hub/internal/kimi"
	"github.com/pashcock/agent-hub/internal/mcpfacade"
	"github.com/pashcock/agent-hub/internal/semantic"
	"github.com/pashcock/agent-hub/internal/store"
	"github.com/pashcock/agent-hub/internal/webui"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-hub: %v\n", err)
		os.Exit(1)
	}
	switch os.Args[1] {
	case "doctor":
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		rep := doctor.Run(ctx, exec.LookPath, runVersion)
		fmt.Printf("listen %s\n\n", cfg.Server.Addr)
		fmt.Print(rep.String())
		fmt.Print(checkHub(ctx, cfg).String())
	case "serve":
		if err := serve(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "agent-hub: %v\n", err)
			os.Exit(1)
		}
	case "pause", "resume":
		if err := providerControl(cfg, os.Args[1], os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "approvals":
		if err := listApprovals(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "agent-hub: %v\n", err)
			os.Exit(1)
		}
	case "approve":
		if err := approve(cfg, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "agent-hub: %v\n", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func serve(cfg *config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dir, err := config.StateDir()
	if err != nil {
		return err
	}

	logPath := filepath.Join(dir, "agent-hub.jsonl")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log %s: %w", logPath, err)
	}
	defer logFile.Close()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logFile, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.Info("agent-hub starting", "addr", cfg.Server.Addr, "state_dir", dir)

	st, err := store.Open(filepath.Join(dir, "hub.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	token, err := loadOrCreateToken(dir)
	if err != nil {
		return err
	}

	adapters := map[string]adapter.Adapter{}
	var names []string
	if cfg.Agents.Codex.Enabled {
		bin, err := exec.LookPath("codex")
		if err != nil {
			return fmt.Errorf("codex is enabled but not on PATH: %w", err)
		}
		ca, err := codex.Open(ctx, bin, dir, "agent-hub",
			codex.AdapterOptions{DefaultCwd: dir, IdleKill: time.Duration(cfg.Agents.Codex.IdleKillS) * time.Second})
		if err != nil {
			return err
		}
		defer ca.Close()
		adapters["codex"] = ca
		names = append(names, "codex")
	}

	if cfg.Agents.Claude.Enabled {
		bin, err := exec.LookPath("claude")
		if err != nil {
			return fmt.Errorf("claude is enabled but not on PATH: %w", err)
		}
		cl := claude.NewAdapter(claude.Options{
			Bin:              bin,
			MaskMCPServer:    "agent-hub",
			MaxLiveProcesses: cfg.Agents.Claude.MaxLiveProcesses,
			IdleKill:         time.Duration(cfg.Agents.Claude.IdleKillS) * time.Second,
		})
		defer cl.Close()
		adapters["claude"] = cl
		names = append(names, "claude")
		slog.Info("claude peer enabled", "bin", bin,
			"max_live_processes", cfg.Agents.Claude.MaxLiveProcesses,
			"idle_kill_s", cfg.Agents.Claude.IdleKillS)
	}

	if cfg.Agents.Grok.Enabled {
		bin, err := exec.LookPath("grok")
		if err != nil {
			return fmt.Errorf("grok is enabled but not on PATH: %w", err)
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		authPath := os.Getenv("GROK_AUTH_PATH")
		if authPath == "" {
			grokHome := os.Getenv("GROK_HOME")
			if grokHome == "" {
				grokHome = filepath.Join(home, ".grok")
			}
			authPath = filepath.Join(grokHome, "auth.json")
		}
		g := grok.NewAdapter(grok.Options{Bin: bin, Home: filepath.Join(dir, "grok"), AuthPath: authPath, MaxLiveProcesses: cfg.Agents.Grok.MaxLiveProcesses, IdleKill: time.Duration(cfg.Agents.Grok.IdleKillS) * time.Second})
		defer g.Close()
		adapters["grok"] = g
		names = append(names, "grok")
		slog.Info("grok peer enabled", "bin", bin)
	}

	if cfg.Agents.Kimi.Enabled {
		bin, err := kimi.ResolveBinary(cfg.Agents.Kimi.Bin)
		if err != nil {
			return fmt.Errorf("kimi is enabled but unavailable: %w", err)
		}
		source := cfg.Agents.Kimi.SourceHome
		if source == "" {
			source = os.Getenv("KIMI_CODE_HOME")
		}
		if source == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			source = filepath.Join(home, ".kimi-code")
		}
		ka := kimi.NewAdapter(kimi.Options{Bin: bin, Home: filepath.Join(dir, "kimi"), SourceHome: source, MaxLiveProcesses: cfg.Agents.Kimi.MaxLiveProcesses, IdleKill: time.Duration(cfg.Agents.Kimi.IdleKillS) * time.Second})
		defer ka.Close()
		adapters["kimi"] = ka
		names = append(names, "kimi")
		slog.Info("kimi peer enabled", "bin", bin)
	}
	h := hub.New(st, adapters, hub.Config{
		MaxParallelPeerTurns: cfg.Limits.MaxParallelPeerTurns,
		HumanGated:           cfg.Permissions.HumanGated,
		ApprovalTimeout:      time.Duration(cfg.Permissions.TimeoutS) * time.Second,
		OnApprovalTimeout:    cfg.Permissions.OnTimeout,
		StallWarn:            time.Duration(cfg.Limits.StallWarnS) * time.Second,
	})
	defer h.Close()
	if err := h.Recover(ctx); err != nil {
		return fmt.Errorf("recovering previous state: %w", err)
	}

	facade := mcpfacade.New(h, names)
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpfacade.RequireToken(token, facade.HTTPHandler()))
	facade.RegisterOperator(mux)
	facade.RegisterProviderOperator(mux, token)

	// The web view carries no token: the listener is loopback-only and
	// hub.db is readable by this user anyway, so a token would guard
	// nothing while adding a step to every page load.
	ui, err := webui.New(h, st)
	if err != nil {
		return err
	}
	if cfg.Semantic.Python != "" || cfg.Semantic.ModelPath != "" {
		worker, err := semantic.New(cfg.Semantic, filepath.Join(dir, "semantic-vectors.db"))
		if err != nil {
			slog.Warn("semantic search unavailable", "error", err)
		} else {
			defer worker.Close()
			ui.Semantic = worker
		}
	}
	ui.Register(mux)
	srv := &http.Server{Addr: cfg.Server.Addr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	fmt.Printf("agent-hub listening on http://%s/mcp\n", cfg.Server.Addr)
	fmt.Printf("web view at            http://%s/\n", cfg.Server.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// loadOrCreateToken returns the shared secret orchestrators present as a
// bearer token. It is the second half of the section 5.3 requirement: the
// MCP config mask hides the hub from a peer, and the token means that even
// an unmasked peer cannot use it, because the hub strips this variable
// from every child environment (see codex.Dial).
func loadOrCreateToken(dir string) (string, error) {
	path := filepath.Join(dir, "token")
	b, err := os.ReadFile(path)
	if err == nil && len(bytes.TrimSpace(b)) >= 32 {
		return string(bytes.TrimSpace(b)), nil
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	tok := hex.EncodeToString(raw[:])
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write token %s: %w", path, err)
	}
	fmt.Printf("wrote a new hub token to %s\n", path)
	return tok, nil
}

func listApprovals(cfg *config.Config) error {
	res, err := http.Get("http://" + cfg.Server.Addr + "/operator/approvals")
	if err != nil {
		return fmt.Errorf("is the daemon running? %w", err)
	}
	defer res.Body.Close()
	var body struct {
		Approvals []struct {
			ID             string `json:"id"`
			ConversationID string `json:"conversation_id"`
			Class          string `json:"class"`
			Command        string `json:"command"`
			Cwd            string `json:"cwd"`
			Reason         string `json:"reason"`
			HumanGated     bool   `json:"human_gated"`
		} `json:"approvals"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return err
	}
	if len(body.Approvals) == 0 {
		fmt.Println("no pending approvals")
		return nil
	}
	for _, a := range body.Approvals {
		gate := ""
		if a.HumanGated {
			gate = "  [needs you]"
		}
		fmt.Printf("%s  %-16s %s%s\n", a.ID, a.Class, a.Command, gate)
		fmt.Printf("    answer: agent-hub approve %s --accept   (or --decline)\n", a.ID)
		if a.Reason != "" {
			fmt.Printf("    reason: %s\n", a.Reason)
		}
		if a.Cwd != "" {
			fmt.Printf("    cwd:    %s\n", a.Cwd)
		}
	}
	return nil
}

func approve(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	conv := fs.String("conversation", "",
		"conversation id; optional, the hub resolves it from the request id")
	accept := fs.Bool("accept", false, "accept the request")
	decline := fs.Bool("decline", false, "decline the request")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: agent-hub approve <request_id> --accept|--decline")
	}
	if *accept == *decline {
		return errors.New("pass exactly one of --accept or --decline")
	}
	decision := "decline"
	if *accept {
		decision = "accept"
	}
	payload, _ := json.Marshal(map[string]string{
		"conversation_id": *conv,
		"request_id":      fs.Arg(0),
		"decision":        decision,
	})
	res, err := http.Post("http://"+cfg.Server.Addr+"/operator/approve",
		"application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("is the daemon running? %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("daemon returned %s: %s", res.Status, bytes.TrimSpace(body))
	}
	fmt.Printf("%s %sd\n", fs.Arg(0), decision)
	return nil
}

// checkHub reports whether an agent started in this environment could
// actually reach the hub. The failure is silent otherwise: an agent
// without the token simply never connects.
func checkHub(ctx context.Context, cfg *config.Config) doctor.HubCheck {
	tokenFile := "(state directory unavailable)"
	if dir, err := config.StateDir(); err == nil {
		tokenFile = filepath.Join(dir, "token")
	}
	return doctor.CheckHub(doctor.HubInputs{
		Addr:      cfg.Server.Addr,
		TokenFile: tokenFile,
		ReadToken: func(path string) (string, error) {
			b, err := os.ReadFile(path)
			return string(bytes.TrimSpace(b)), err
		},
		Getenv: os.Getenv,
		Probe: func(url, token string) (int, error) {
			probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
			if err != nil {
				return 0, err
			}
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				return 0, err
			}
			defer res.Body.Close()
			return res.StatusCode, nil
		},
	})
}

func runVersion(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: agent-hub <command>

commands:
  doctor     report detected agents, versions and protocols
  serve      run the daemon
  pause      pause <agent> [--reason text] [--for 5h]
  resume     resume <agent>
  approvals  list permission requests waiting for you
  approve    answer one: approve <request_id> --accept|--decline
`)
}
