// Command memos-build produces a release binary of this fork without Docker: it
// builds the SPA into server/frontend/dist (which go:embed bakes in) and then
// builds ./cmd/memos.
//
//	go run cmd/memos-build/main.go            # -> ./memos
//	go run cmd/memos-build/main.go -o memos-md
//
// It is a development helper for source checkouts. The published binary is
// ./cmd/memos, which is what the Dockerfile and the release workflow build.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func main() {
	output := flag.String("o", "memos", "output binary path")
	skipFrontend := flag.Bool("skip-frontend", false, "reuse the existing server/frontend/dist")
	flag.Parse()

	root, err := moduleRoot()
	if err != nil {
		fail(err)
	}

	if !*skipFrontend {
		run(root, "pnpm", "install")
		run(filepath.Join(root, "web"), "pnpm", "release")
	}

	binary := *output
	if !filepath.IsAbs(binary) {
		binary = filepath.Join(root, binary)
	}
	if runtime.GOOS == "windows" && filepath.Ext(binary) == "" {
		binary += ".exe"
	}
	run(root, "go", "build", "-trimpath", "-o", binary, "./cmd/memos")
	fmt.Printf("\nbuilt %s\n", binary)
	fmt.Println("run it with: " + filepath.Base(binary) + " --memo-files --port 5230 --data ./data")
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

func run(dir string, name string, args ...string) {
	command := exec.Command(name, args...)
	command.Dir = dir
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Stdin = os.Stdin
	fmt.Printf("==> %s %v (in %s)\n", name, args, dir)
	if err := command.Run(); err != nil {
		fail(fmt.Errorf("%s %v: %w", name, args, err))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
