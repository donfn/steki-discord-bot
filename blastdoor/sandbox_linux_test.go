//go:build linux && (amd64 || arm64)

package blastdoor

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func toolEnv() []string { return []string{"PATH=" + os.Getenv("PATH")} }

func TestCheckSandbox(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd not installed")
	}
	if err := CheckSandbox(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// TestSandboxCutsTheNetwork fetches a page with and without the sandbox, also
// through a shell, as an exploit could exec one.
func TestSandboxCutsTheNetwork(t *testing.T) {
	if _, err := exec.LookPath("wget"); err != nil {
		t.Skip("wget not installed")
	}
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer ts.Close()

	if msg, err := exec.Command("wget", "-q", "-T", "3", "-O", "-", ts.URL).CombinedOutput(); err != nil {
		t.Skipf("wget cannot reach the test server unsandboxed: %v %s", err, msg)
	}
	hits.Store(0)
	for name, args := range map[string][]string{
		"direct":        {"wget", "-q", "-T", "3", "-O", "-", ts.URL},
		"through shell": {"/bin/sh", "-c", "wget -q -T 3 -O - " + ts.URL},
	} {
		err := sandboxCommand(t.Context(), heifMemoryKiB, toolEnv(), args[0], args[1:]...).Run()
		var exit *exec.ExitError
		switch {
		case err == nil:
			t.Errorf("%s: fetched a page inside the sandbox", name)
		case !errors.As(err, &exit) || exit.ExitCode() == sandboxFailedExit:
			t.Errorf("%s: the sandbox itself failed, so this proves nothing: %v", name, err)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("the server saw %d requests from inside the sandbox", hits.Load())
	}
}

func TestSandboxAppliesTheMemoryLimit(t *testing.T) {
	out, err := sandboxCommand(t.Context(), heifMemoryKiB, toolEnv(), "/bin/sh", "-c", "ulimit -v").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != strconv.Itoa(heifMemoryKiB) {
		t.Errorf("ulimit -v = %s, want %d", got, heifMemoryKiB)
	}
}

func TestSandboxPassesOnlyTheGivenEnvironment(t *testing.T) {
	t.Setenv("STEKI_TEST_SECRET", "x")
	out, err := sandboxCommand(t.Context(), heifMemoryKiB, toolEnv(), "/bin/sh", "-c", "echo [$STEKI_TEST_SECRET]").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "[]" {
		t.Errorf("environment leaked: %s", out)
	}
}

func TestSandboxReportsAMissingTool(t *testing.T) {
	if err := sandboxCommand(t.Context(), heifMemoryKiB, toolEnv(), "no-such-tool").Run(); err == nil {
		t.Error("ran a tool that does not exist")
	}
}
