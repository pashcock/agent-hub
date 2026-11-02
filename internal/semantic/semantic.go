// Package semantic ranks recorded messages using an offline model2vec worker.
package semantic

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pashcock/agent-hub/internal/store"
)

//go:embed worker.py
var workerScript string

type Config struct {
	Python    string `toml:"python"`
	ModelPath string `toml:"model_path"`
}

type Hit struct {
	ConversationID string  `json:"conversation_id"`
	Score          float64 `json:"similarity"`
	Exact          bool    `json:"exact"`
	Snippet        string  `json:"snippet"`
}

type Worker struct {
	cfg    Config
	cache  string
	gate   chan struct{}
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *json.Decoder
	closed bool
}

func New(cfg Config, cache string) (*Worker, error) {
	if !filepath.IsAbs(cfg.Python) || !filepath.IsAbs(cfg.ModelPath) {
		return nil, errors.New("semantic python and model_path must be absolute local paths")
	}
	if info, err := os.Stat(cfg.Python); err != nil || info.IsDir() {
		return nil, errors.New("semantic Python executable is missing")
	}
	for _, name := range []string{"model.safetensors", "config.json", "tokenizer.json"} {
		if _, err := os.Stat(filepath.Join(cfg.ModelPath, name)); err != nil {
			return nil, fmt.Errorf("semantic local model: %w", err)
		}
	}
	return &Worker{cfg: cfg, cache: cache, gate: make(chan struct{}, 1)}, nil
}
func (w *Worker) start() error {
	cmd := exec.Command(w.cfg.Python, "-u", "-c", workerScript, w.cfg.ModelPath, w.cache)
	cmd.Env = append(os.Environ(), "HF_HUB_OFFLINE=1", "HF_HUB_DISABLE_TELEMETRY=1", "TOKENIZERS_PARALLELISM=false")
	cmd.Stderr = os.Stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		return err
	}
	if err = cmd.Start(); err != nil {
		input.Close()
		output.Close()
		return err
	}
	w.cmd, w.input, w.output = cmd, input, json.NewDecoder(output)
	return nil
}
func (w *Worker) stop() {
	if w.cmd != nil {
		w.input.Close()
		w.cmd.Process.Kill()
		w.cmd.Wait()
		w.cmd = nil
	}
}
func (w *Worker) Close() {
	w.gate <- struct{}{}
	defer func() { <-w.gate }()
	w.closed = true
	w.stop()
}
func (w *Worker) Scores(ctx context.Context, query string, texts []string) ([]float64, error) {
	select {
	case w.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-w.gate }()
	if w.closed {
		return nil, errors.New("semantic worker closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if w.cmd == nil {
		if err := w.start(); err != nil {
			return nil, err
		}
	}
	var response struct {
		Scores []float64 `json:"scores"`
		Error  string    `json:"error"`
	}
	done := make(chan error, 1)
	go func() {
		if err := json.NewEncoder(w.input).Encode(map[string]any{"query": query, "texts": texts}); err != nil {
			done <- err
			return
		}
		done <- w.output.Decode(&response)
	}()
	select {
	case <-ctx.Done():
		w.stop()
		<-done
		return nil, ctx.Err()
	case err := <-done:
		if err != nil {
			w.stop()
			return nil, err
		}
	}
	if response.Error != "" {
		return nil, errors.New(response.Error)
	}
	if len(response.Scores) != len(texts) {
		return nil, errors.New("semantic score count mismatch")
	}
	for _, score := range response.Scores {
		if math.IsNaN(score) || math.IsInf(score, 0) {
			return nil, errors.New("invalid semantic score")
		}
	}
	return response.Scores, nil
}

type Scorer interface {
	Scores(context.Context, string, []string) ([]float64, error)
}

// Rank keeps literal hits first, then ranks meaning matches by cosine. One best
// chunk per conversation prevents long conversations dominating the results.
func Rank(ctx context.Context, scorer Scorer, query string, docs []store.SearchDocument) ([]Hit, error) {
	if len([]rune(query)) > 2048 {
		return nil, errors.New("search query exceeds 2048 characters")
	}
	texts := make([]string, len(docs))
	for i, d := range docs {
		texts[i] = d.Text
	}
	if len(docs) == 0 {
		return []Hit{}, nil
	}
	scores, err := scorer.Scores(ctx, query, texts)
	if err != nil {
		return nil, err
	}
	if len(scores) != len(docs) {
		return nil, errors.New("semantic score count mismatch")
	}
	best := map[string]Hit{}
	q := strings.ToLower(query)
	for i, d := range docs {
		exact := d.LiteralSnippet != "" || strings.Contains(strings.ToLower(d.Text), q) || strings.Contains(strings.ToLower(d.Metadata), q)
		if !exact && scores[i] < 0.25 {
			continue
		}
		snippet := d.Text
		if d.LiteralSnippet != "" {
			snippet = d.LiteralSnippet
		}
		// Center literal matches so the displayed evidence explains the result.
		if pos := strings.Index(strings.ToLower(snippet), q); pos >= 0 {
			start := len([]rune(strings.ToLower(snippet)[:pos])) - 120
			if start < 0 {
				start = 0
			}
			snippet = string([]rune(snippet)[start:])
		}
		runes := []rune(snippet)
		if len(runes) > 420 {
			snippet = string(runes[:420]) + "…"
		}
		hit := Hit{d.ConversationID, scores[i], exact, snippet}
		old, ok := best[d.ConversationID]
		if !ok || (hit.Exact && !old.Exact) || hit.Exact == old.Exact && hit.Score > old.Score {
			best[d.ConversationID] = hit
		}
	}
	out := make([]Hit, 0, len(best))
	for _, h := range best {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Exact != out[j].Exact {
			return out[i].Exact
		}
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ConversationID < out[j].ConversationID
	})
	return out, nil
}
