package doctor

import "fmt"

// HubInputs are the pieces CheckHub needs. They are function values so
// the check can be tested without a daemon, a file or a network.
type HubInputs struct {
	Addr      string
	TokenFile string
	ReadToken func(path string) (string, error)
	Getenv    func(name string) string
	// Probe performs one request against the endpoint with the given
	// bearer token and returns the HTTP status.
	Probe func(url, token string) (int, error)
}

// TokenEnvVar is the variable orchestrator agents read at run time.
const TokenEnvVar = "AGENT_HUB_TOKEN"

// HubCheck reports whether an agent started here could actually reach the
// hub.
//
// This exists because the failure is silent. An agent whose token is
// missing or stale does not error: its MCP entry simply reports `failed`
// and the peer tools are absent, which looks exactly like the hub not
// being configured at all.
type HubCheck struct {
	Addr          string
	TokenFile     string
	TokenReadable bool
	EnvSet        bool
	EnvMatches    bool
	Reachable     bool
	Authorized    bool
	Status        int
	Err           string
}

func CheckHub(in HubInputs) HubCheck {
	c := HubCheck{Addr: in.Addr, TokenFile: in.TokenFile}

	token, err := in.ReadToken(in.TokenFile)
	c.TokenReadable = err == nil && token != ""

	env := in.Getenv(TokenEnvVar)
	c.EnvSet = env != ""
	c.EnvMatches = c.EnvSet && c.TokenReadable && env == token

	// Probe with whatever an agent would actually send.
	status, perr := in.Probe("http://"+in.Addr+"/mcp", env)
	if perr != nil {
		c.Err = perr.Error()
	} else {
		c.Reachable = true
		c.Status = status
		// 401 is the only status that means "rejected". Anything else,
		// including the 400 a bare GET earns from the MCP handler, means
		// the credentials were accepted.
		c.Authorized = status != 401
	}
	return c
}

// Problem returns one line naming the first thing that would stop an
// agent reaching the hub, or "" when nothing would.
func (c HubCheck) Problem() string {
	switch {
	case !c.TokenReadable:
		return fmt.Sprintf("no token at %s; the daemon has never started. Run: systemctl --user start agent-hub",
			c.TokenFile)
	case !c.Reachable:
		return fmt.Sprintf("the daemon is not running on %s (%s). Run: systemctl --user start agent-hub",
			c.Addr, c.Err)
	case !c.EnvSet:
		return fmt.Sprintf("%s is not set in this environment, so agents started here cannot reach the hub and will fail silently. Put it in ~/.config/environment.d/agent-hub.conf",
			TokenEnvVar)
	case !c.EnvMatches:
		return fmt.Sprintf("%s does not match %s; the token was regenerated. Update ~/.config/environment.d/agent-hub.conf and log in again",
			TokenEnvVar, c.TokenFile)
	case !c.Authorized:
		return fmt.Sprintf("the hub rejected the token with HTTP 401")
	}
	return ""
}

func (c HubCheck) String() string {
	line := fmt.Sprintf("hub      %-10s %-12s %s", statusWord(c), "", c.Addr)
	if p := c.Problem(); p != "" {
		return line + "\n         " + p + "\n"
	}
	return line + "\n"
}

func statusWord(c HubCheck) string {
	switch {
	case !c.Reachable:
		return "down"
	case c.Problem() != "":
		return "degraded"
	default:
		return "ok"
	}
}
