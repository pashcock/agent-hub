// Package doctor reports which agent CLIs are installed, their versions
// and which protocol agent-hub would use to drive them.
package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// LookPathFunc resolves a binary name to an absolute path.
type LookPathFunc func(bin string) (string, error)

// RunFunc executes a binary and returns its combined output.
type RunFunc func(ctx context.Context, bin string, args ...string) (string, error)

type Finding struct {
	Agent     string
	Installed bool
	Path      string
	Version   string
	Protocol  string
	Note      string
}

type Report struct {
	Findings []Finding
}

func (r Report) Find(agent string) *Finding {
	for i := range r.Findings {
		if r.Findings[i].Agent == agent {
			return &r.Findings[i]
		}
	}
	return nil
}

func (r Report) String() string {
	var b strings.Builder
	for _, f := range r.Findings {
		if !f.Installed {
			fmt.Fprintf(&b, "%-8s not installed\n", f.Agent)
			continue
		}
		fmt.Fprintf(&b, "%-8s %-10s %-12s %s\n", f.Agent, f.Version, f.Protocol, f.Path)
		if f.Note != "" {
			fmt.Fprintf(&b, "         %s\n", f.Note)
		}
	}
	return b.String()
}

type probe struct {
	agent       string
	bin         string
	versionArgs []string
	protocol    string
	note        string
}

var probes = []probe{
	{"codex", "codex", []string{"--version"}, "app-server", ""},
	{"claude", "claude", []string{"--version"}, "stream-json", ""},
	{"kimi", "kimi", []string{"--version"}, "acp", ""},
	{"grok", "grok", []string{"--version"}, "acp", ""},
}

// Run probes every known agent. It never returns an error: a missing or
// broken binary is a finding, not a failure.
func Run(ctx context.Context, look LookPathFunc, run RunFunc) Report {
	rep := Report{}
	for _, p := range probes {
		f := Finding{Agent: p.agent, Protocol: p.protocol, Note: p.note}
		path, err := look(p.bin)
		if err != nil && p.agent == "kimi" {
			if home, e := os.UserHomeDir(); e == nil {
				path, err = look(filepath.Join(home, ".kimi-code", "bin", "kimi"))
			}
		}
		if err != nil {
			rep.Findings = append(rep.Findings, f)
			continue
		}
		f.Installed = true
		f.Path = path
		out, err := run(ctx, path, p.versionArgs...)
		if err != nil {
			f.Note = fmt.Sprintf("version probe failed: %v", err)
		} else {
			f.Version = parseVersion(out)
			if note := DriftNote(p.agent, f.Version); note != "" {
				f.Note = note
			}
		}
		rep.Findings = append(rep.Findings, f)
	}
	return rep
}

var semverRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

func parseVersion(out string) string {
	return semverRe.FindString(out)
}
