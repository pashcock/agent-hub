package codex

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildFakeServer compiles the fixture once per test binary run.
func buildFakeServer(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fakeserver")
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/fakeserver")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building fakeserver: %v\n%s", err, out)
	}
	return bin
}

func dialFake(t *testing.T) *Conn {
	t.Helper()
	bin := buildFakeServer(t)
	c, err := Dial(context.Background(), bin, nil, t.TempDir())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestCallRoundTrip(t *testing.T) {
	c := dialFake(t)
	var got map[string]string
	if err := c.Call(context.Background(), "echo", map[string]string{"a": "b"}, &got); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got["a"] != "b" {
		t.Errorf("echo returned %v, want a=b", got)
	}
}

func TestCallPropagatesRPCError(t *testing.T) {
	c := dialFake(t)
	err := c.Call(context.Background(), "boom", nil, nil)
	if err == nil {
		t.Fatal("Call must return the JSON-RPC error")
	}
	if !strings.Contains(err.Error(), "boom happened") {
		t.Errorf("error = %v, want it to carry the server message", err)
	}
}

func TestNotificationsAreDelivered(t *testing.T) {
	c := dialFake(t)
	seen := make(chan string, 4)
	c.OnNotification(func(method string, _ json.RawMessage) { seen <- method })

	if err := c.Call(context.Background(), "notify", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-seen:
		if m != "thing/happened" {
			t.Errorf("notification method = %q", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the notification")
	}
}

func TestServerRequestIsRoutedAndAnswerable(t *testing.T) {
	c := dialFake(t)
	c.OnServerRequest(func(id json.RawMessage, method string, params json.RawMessage) {
		if method != "needApproval" {
			t.Errorf("server request method = %q", method)
		}
		if err := c.Respond(id, map[string]string{"decision": "decline"}); err != nil {
			t.Errorf("Respond: %v", err)
		}
	})

	var got struct {
		DecisionEcho struct {
			Decision string `json:"decision"`
		} `json:"decisionEcho"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Call(ctx, "ask", nil, &got); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.DecisionEcho.Decision != "decline" {
		t.Errorf("server saw decision %q, want decline", got.DecisionEcho.Decision)
	}
}

func TestCallRespectsContextCancellation(t *testing.T) {
	c := dialFake(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// "ask" never completes because no OnServerRequest handler is set.
	err := c.Call(ctx, "ask", nil, nil)
	if err == nil {
		t.Fatal("Call must fail when its context expires")
	}
}

func TestCloseUnblocksPendingCalls(t *testing.T) {
	c := dialFake(t)
	errCh := make(chan error, 1)
	go func() { errCh <- c.Call(context.Background(), "ask", nil, nil) }()
	time.Sleep(200 * time.Millisecond)
	c.Close()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("a pending call must fail once the connection closes")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close left a call hanging")
	}
}
