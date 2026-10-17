// SPDX-License-Identifier: AGPL-3.0-or-later

// Command watch rebuilds and restarts the server when Go source changes.
//
// `make dev` ran `go run ./cmd/lexarii`, which builds once and then serves
// the same binary until somebody notices. Nothing said so, and the failure is
// silent and convincing: the code is right, the test passes, the endpoint
// answers the old way, and the obvious conclusion is that the code is wrong.
// It cost three wrong diagnoses in one afternoon - an endpoint declared live
// that did not exist yet, and a path declared safe that was serving somebody's
// whole library to anybody who asked.
//
// Polling rather than inotify, deliberately. fsnotify is a dependency, a
// watcher is a development convenience, and every dependency here has to
// justify itself in NOTICE. Walking a few hundred files twice a second costs
// nothing measurable and works the same on every machine.
package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// interval is how often the tree is checked.
//
// Fast enough that saving a file and reaching for the browser is the same
// gesture; slow enough to be invisible.
const interval = 500 * time.Millisecond

// settle waits for an editor to finish writing.
//
// A save can be several writes, and rebuilding halfway through one produces a
// compile error for source that is fine a millisecond later.
const settle = 150 * time.Millisecond

func main() {
	target := "./cmd/lexarii"
	if len(os.Args) > 1 {
		target = os.Args[1]
	}

	binary := filepath.Join(os.TempDir(), "lexarii-dev")
	server := &server{target: target, binary: binary}

	// Ctrl-C has to reach the child, or every run leaves a server holding the
	// port and the next one fails to bind.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		server.stop()
		os.Exit(0)
	}()

	server.restart()
	last := snapshot()

	for {
		time.Sleep(interval)
		now := snapshot()
		if sameTree(last, now) {
			continue
		}
		time.Sleep(settle)
		now = snapshot()
		last = now

		fmt.Println("\n── change detected, rebuilding ──")
		server.restart()
	}
}

type server struct {
	target  string
	binary  string
	running *exec.Cmd

	// stopping is set while a restart is killing the old process, so its exit
	// is not reported as a crash.
	mu       sync.Mutex
	stopping bool
}

// restart builds, and only then stops what is running.
//
// That order matters: a build that fails leaves the previous server up and
// answering, so a typo costs an error message rather than a dead port and a
// browser full of connection refusals.
func (s *server) restart() {
	build := exec.Command("go", "build", "-o", s.binary, s.target)
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Println("── build failed; the running server is untouched ──")
		return
	}

	s.stop()

	run := exec.Command(s.binary)
	run.Stdout, run.Stderr, run.Stdin = os.Stdout, os.Stderr, os.Stdin
	// Its own process group, so stopping it stops anything it started.
	run.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := run.Start(); err != nil {
		fmt.Println("starting the server:", err)
		return
	}
	s.running = run

	// Say so when the server dies on its own.
	//
	// Without this the watcher sits there looking like it is working while
	// nothing is listening, and the commonest reason is the most confusing:
	// another copy of the server is already holding the port, so the new one
	// exits on its first breath and the only evidence is one line that has
	// already scrolled past.
	go func() {
		err := run.Wait()
		s.mu.Lock()
		stopping := s.stopping
		s.mu.Unlock()
		if stopping {
			return
		}
		fmt.Println("\n── the server exited on its own ──")
		if err != nil {
			fmt.Println("  ", err)
		}
		fmt.Println("   Nothing is listening now. If this says \"address already in use\",")
		fmt.Println("   another server is running: `pgrep -af lexarii` will find it.")
		fmt.Println("   Saving a file will build and start it again.")
	}()
}

func (s *server) stop() {
	if s.running == nil || s.running.Process == nil {
		return
	}
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.stopping = false
		s.mu.Unlock()
	}()
	// The group, not the process: a server that spawned workers should not
	// leave them behind holding a database connection.
	_ = syscall.Kill(-s.running.Process.Pid, syscall.SIGTERM)

	// Waited on by the goroutine started in restart, so this waits for the
	// process to disappear rather than reaping it twice.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(s.running.Process.Pid, 0); err != nil {
			break // gone
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(-s.running.Process.Pid, syscall.SIGKILL)
	s.running = nil
}

// snapshot records the modification time of every file that can change what
// the binary does.
func snapshot() map[string]time.Time {
	out := map[string]time.Time{}
	_ = filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !watched(path) {
			return nil
		}
		if info, err := d.Info(); err == nil {
			out[path] = info.ModTime()
		}
		return nil
	})
	return out
}

func skipDir(name string) bool {
	switch name {
	// node_modules alone is tens of thousands of files and cannot affect the
	// Go binary; web/build is written by the client's own dev server, which
	// has its own reload and does not need this one.
	case ".git", "node_modules", "build", ".svelte-kit", "bin", "dist", "data":
		return true
	}
	return false
}

// watched is what the SERVER is built from. The client reloads itself.
func watched(path string) bool {
	switch {
	case strings.HasSuffix(path, ".go"):
		return true
	// Migrations and prompts are embedded, so changing one changes the binary
	// while leaving every .go file untouched - which looks exactly like the
	// bug this command exists to prevent.
	case strings.HasSuffix(path, ".sql"), strings.HasSuffix(path, ".md") && strings.Contains(path, "prompts"):
		return true
	default:
		return false
	}
}

func sameTree(a, b map[string]time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for path, at := range a {
		if bt, ok := b[path]; !ok || !bt.Equal(at) {
			return false
		}
	}
	return true
}
