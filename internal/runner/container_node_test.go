package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// fakeNodeDist serves a Node.js release for every os-arch that nodejs.org builds. Each bin/node is a shell script that
// prints its os-arch, so a test can tell which build a job ran even though the fake engine runs it on this machine.
func fakeNodeDist(t *testing.T, ver string) (requested func() []string) {
	t.Helper()
	builds := []string{"linux-x64", "linux-arm64", "darwin-x64", "darwin-arm64"}
	tarballs := map[string][]byte{}
	var sums strings.Builder
	for _, b := range builds {
		file := fmt.Sprintf("node-v%s-%s.tar.gz", ver, b)
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		script := "#!/bin/sh\necho \"fake node " + b + "\"\n"
		top := strings.TrimSuffix(file, ".tar.gz")
		tw.WriteHeader(&tar.Header{Name: top + "/bin/node", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(script))})
		tw.Write([]byte(script))
		tw.Close()
		gz.Close()
		tarballs[file] = buf.Bytes()
		sum := sha256.Sum256(buf.Bytes())
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), file)
	}
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/index.json":
			fmt.Fprintf(w, `[{"version":"v%s"}]`, ver)
		case r.URL.Path == "/v"+ver+"/SHASUMS256.txt":
			w.Write([]byte(sums.String()))
		case tarballs[strings.TrimPrefix(r.URL.Path, "/v"+ver+"/")] != nil:
			file := strings.TrimPrefix(r.URL.Path, "/v"+ver+"/")
			mu.Lock()
			got = append(got, file)
			mu.Unlock()
			w.Write(tarballs[file])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := nodeDist
	nodeDist = srv.URL
	t.Cleanup(func() { nodeDist = old })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

const containerNodeWorkflow = `on: push
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - run: echo "tool cache $RUNNER_TOOL_CACHE"
      - run: echo "runner ${{ runner.os }} ${{ runner.arch }} env $RUNNER_OS $RUNNER_ARCH"
      - if: runner.os == 'Linux'
        run: echo "linux-only step $((40+2))"
      - uses: ./act
`

var containerNodeAction = map[string]string{
	"act/action.yml": "name: n\nruns:\n  using: node24\n  main: main.js\n",
	"act/main.js":    "console.log('main')\n",
}

// A JavaScript action in a container job must get the Node.js build of the container's platform, not the one of this
// machine, and the tool cache of the job must be the one of that platform.
func TestContainerJobNodeMatchesThePlatform(t *testing.T) {
	bin, _ := fakeEngine(t)
	requested := fakeNodeDist(t, "24.99.0")
	dir := actionRepo(t, containerNodeWorkflow, containerNodeAction)
	state := os.Getenv("JACTIONLINT_CACHE_DIR")

	// both platforms in one cache: the second run must not reuse the Node.js of the first
	for _, c := range []struct{ platform, build, arch string }{
		{"linux/amd64", "linux-x64", "X64"},
		{"linux/arm64", "linux-arm64", "ARM64"},
	} {
		out := mustRun(t, dir, 0, "--container-engine", bin, "--container", "--platform", c.platform)
		if !strings.Contains(out, "fake node "+c.build) {
			t.Errorf("--platform %s: the action did not run the %s build of Node.js\n%s", c.platform, c.build, out)
		}
		wantCache := filepath.Join(state, "toolcache", strings.ReplaceAll(c.platform, "/", "-"))
		if !strings.Contains(out, "tool cache "+wantCache+"\n") {
			t.Errorf("--platform %s: RUNNER_TOOL_CACHE is not %s\n%s", c.platform, wantCache, out)
		}
		// the job is a Linux runner of that architecture, in expressions and in the environment
		if want := "runner Linux " + c.arch + " env Linux " + c.arch + "\n"; !strings.Contains(out, want) {
			t.Errorf("--platform %s: the runner context is not %q\n%s", c.platform, want, out)
		}
		if !strings.Contains(out, "linux-only step 42") {
			t.Errorf("--platform %s: a step with if: runner.os == 'Linux' was skipped\n%s", c.platform, out)
		}
	}
	want := []string{"node-v24.99.0-linux-x64.tar.gz", "node-v24.99.0-linux-arm64.tar.gz"}
	if got := requested(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("downloaded %v, want %v", got, want)
	}
	// nothing of the containers lands in the tool cache of this machine
	host := runtime.GOOS + "-" + runtime.GOARCH
	if host != "linux-amd64" && host != "linux-arm64" {
		if _, err := os.Stat(filepath.Join(state, "toolcache", host, "node")); err == nil {
			t.Errorf("a container job wrote Node.js into the tool cache of this machine (%s)", host)
		}
	}
}

// Without --platform the platform of the pulled image decides, so the test picks an architecture this machine does not
// have: the host's build of Node.js would be wrong on every OS.
func TestContainerJobNodeFollowsTheImage(t *testing.T) {
	bin, _ := fakeEngine(t)
	requested := fakeNodeDist(t, "24.99.0")
	platform, build := "linux/arm64", "linux-arm64"
	if runtime.GOARCH == "arm64" {
		platform, build = "linux/amd64", "linux-x64"
	}
	t.Setenv("FAKE_DOCKER_PLATFORM", platform)
	dir := actionRepo(t, containerNodeWorkflow, containerNodeAction)
	out := mustRun(t, dir, 0, "--container-engine", bin, "--container")
	if !strings.Contains(out, "fake node "+build) {
		t.Errorf("an image of %s did not get the %s build of Node.js\n%s", platform, build, out)
	}
	if got := requested(); len(got) != 1 || got[0] != "node-v24.99.0-"+build+".tar.gz" {
		t.Errorf("downloaded %v, want the %s build only", got, build)
	}
}

func TestNodePlatformOf(t *testing.T) {
	for in, want := range map[string]nodePlatform{
		"linux/amd64":    {"linux", "x64"},
		"linux/arm64":    {"linux", "arm64"},
		"linux/arm64/v8": {"linux", "arm64"},
		"linux/arm/v7":   {"linux", "armv7l"},
		"linux/386":      {"linux", "x86"},
		"darwin/arm64":   {"darwin", "arm64"},
	} {
		if got := nodePlatformOf(in); got != want {
			t.Errorf("nodePlatformOf(%q) = %v, want %v", in, got, want)
		}
	}
}
