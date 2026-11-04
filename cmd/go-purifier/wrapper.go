// SPDX-License-Identifier: MIT

package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

// realGo is the go command this binary runs: GO_PURIFIER_GO, or the first
// go on PATH that is not this binary.
func realGo() (string, error) {
	if g := os.Getenv("GO_PURIFIER_GO"); g != "" {
		return g, nil
	}
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		candidate := filepath.Join(dir, "go")
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(candidate); err == nil && resolved == self {
			continue
		}
		return candidate, nil
	}
	return "", errors.New("no go command found; set GO_PURIFIER_GO")
}

// runGo runs the real go command, with GOPROXY pointing at the locked
// downloads when the build has a current lockfile.
func runGo(args []string) int {
	goBin, err := realGo()
	if err != nil {
		fmt.Fprintf(os.Stderr, "go-purifier: %v\n", err)
		return 1
	}
	if os.Getenv("GO_PURIFIER") != "0" {
		dir, err := workingDir(args)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if root, lock, err := findGopathRoot(dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		} else if lock != nil {
			return runGopathGo(goBin, dir, root, lock, args)
		}
	}
	env := os.Environ()
	proxyDir := ""
	if os.Getenv("GO_PURIFIER") != "0" {
		dir, err := workingDir(args)
		if err != nil {
			fmt.Fprintf(os.Stderr, "go-purifier: %v\n", err)
			return 1
		}
		var extra []string
		extra, proxyDir, err = interactiveEnv(goBin, dir, changesDependencies(args))
		if err != nil {
			fmt.Fprintf(os.Stderr, "go-purifier: %v\n", err)
			return 1
		}
		env = append(env, extra...)
	}
	if proxyDir == "" {
		if err := syscall.Exec(goBin, append([]string{"go"}, args...), env); err != nil {
			fmt.Fprintf(os.Stderr, "go-purifier: running %s: %v\n", goBin, err)
			return 1
		}
		return 0
	}
	dir, err := workingDir(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	effective := args
	if len(effective) >= 2 && effective[0] == "-C" {
		effective = effective[2:]
	} else if len(effective) > 0 && strings.HasPrefix(effective[0], "-C=") {
		effective = effective[1:]
	}
	if len(effective) > 0 && (effective[0] == "get" || (len(effective) >= 2 && effective[0] == "mod" && effective[1] == "tidy")) {
		if root, ok := findLockRoot(dir); ok {
			if lock, err := readLock(lockPathAt(root, "")); err == nil && len(lock.ProjectInputs) > 0 {
				inputs, err := resolveModuleInputs(root, lock.ProjectInputs, nil)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return 1
				}
				module, ok := findUp(dir, "go.mod")
				if !ok {
					fmt.Fprintln(os.Stderr, "go-purifier: no go.mod for dependency editing")
					return 1
				}
				alternate, cleanup, err := moduleEdit(module, inputs.Repositories)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return 1
				}
				patterns, err := moduleInputPatterns(inputs.Repositories)
				if err != nil {
					cleanup(false)
					fmt.Fprintln(os.Stderr, err)
					return 1
				}
				env = append(env, "GOWORK=off", "GOFLAGS="+os.Getenv("GOFLAGS")+" -modfile="+alternate, "GONOSUMDB="+strings.Trim(os.Getenv("GONOSUMDB")+","+patterns, ","))
				status := runWithHint(goBin, args, env, proxyDir)
				if err := cleanup(status == 0); err != nil {
					fmt.Fprintln(os.Stderr, err)
					return 1
				}
				return status
			}
		}
	}
	return runWithHint(goBin, args, env, proxyDir)
}

// runWithHint runs the go command as a child process, so that when it
// fails after reading the proxy tree, which means it needed a download the
// lockfile does not hold, a hint can say what to run. The terminal sends
// interrupts to the child as well; the wrapper waits for the child's exit
// and forwards a termination request.
func runWithHint(goBin string, args, env []string, proxyDir string) int {
	cmd := exec.Command(goBin, args...)
	cmd.Args[0] = "go"
	cmd.Env = env
	cmd.Stdin, cmd.Stdout = os.Stdin, os.Stdout
	var stderr tailBuffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	signal.Ignore(syscall.SIGINT, syscall.SIGQUIT)
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM, syscall.SIGHUP)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "go-purifier: running %s: %v\n", goBin, err)
		return 1
	}
	go func() {
		for s := range terms {
			_ = cmd.Process.Signal(s)
		}
	}()
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	if strings.Contains(stderr.String(), proxyDir) {
		fmt.Fprintln(os.Stderr, "go-purifier: the go command needed a download that go-purifier.lock.json does not hold. Run `go-purifier lock` and commit the lockfile.")
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() > 0 {
		return exit.ExitCode()
	}
	return 1
}

// tailBuffer keeps the last 64 KiB written to it.
type tailBuffer struct{ data []byte }

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	if over := len(b.data) - 64<<10; over > 0 {
		b.data = b.data[over:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string { return string(b.data) }

// changesDependencies reports whether a go command line adds, upgrades or
// looks up module versions, as `go get`, `go mod tidy` and `go install
// pkg@version` do. Such a command may need modules the lockfile does not
// hold yet, so it reads the locked downloads first and the user's
// GOPROXY after them.
func changesDependencies(args []string) bool {
	if len(args) >= 2 && args[0] == "-C" {
		args = args[2:]
	} else if len(args) >= 1 && strings.HasPrefix(args[0], "-C=") {
		args = args[1:]
	}
	if len(args) == 0 {
		return false
	}
	rest := args[1:]
	hasVersion := func() bool {
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") && strings.Contains(a, "@") {
				return true
			}
		}
		return false
	}
	switch args[0] {
	case "get":
		return true
	case "mod":
		return len(rest) > 0 && (rest[0] == "tidy" || rest[0] == "download" || rest[0] == "why")
	case "work":
		return len(rest) > 0 && rest[0] == "sync"
	case "install", "run":
		return hasVersion()
	case "list":
		for _, a := range rest {
			if a == "-u" || a == "-versions" || a == "-retracted" {
				return true
			}
		}
		return hasVersion()
	}
	return false
}

// workingDir is the directory the go command runs in: the directory of a
// leading -C flag, which the go command accepts only as the first flag, or
// the current directory.
func workingDir(args []string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	var dir string
	switch {
	case len(args) >= 2 && args[0] == "-C":
		dir = args[1]
	case len(args) >= 1 && strings.HasPrefix(args[0], "-C="):
		dir = strings.TrimPrefix(args[0], "-C=")
	default:
		return cwd, nil
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(cwd, dir)
	}
	return dir, nil
}

// noProxyMatchesNothing is a GONOPROXY pattern that matches no module. It
// keeps GOPRIVATE, which GONOPROXY defaults to, from sending the go command
// past the locked downloads to version control.
const noProxyMatchesNothing = "go-purifier.invalid"

// interactiveEnv is what the go command's environment gains when it runs
// in dir, inside a build with a lockfile, and the proxy tree it reads. For
// most commands the locked downloads are the only proxy, and a lockfile
// that is stale or of another format version stops them with the command
// to run. A command that changes the dependencies reads the locked
// downloads first and the user's GOPROXY after them, with the user's
// GONOPROXY and GOSUMDB, so that it can add modules the lockfile does not
// hold yet; once the lockfile is stale, it resolves the usual way. A build
// without a lockfile resolves the usual way.
func interactiveEnv(goBin, dir string, changing bool) ([]string, string, error) {
	root, ok := findLockRoot(dir)
	if !ok {
		fmt.Fprintln(os.Stderr, "go-purifier: warning: no go-purifier.lock.json found; running go impurely. Run `go-purifier lock` to use Nix-resolved dependencies.")
		return nil, "", nil
	}
	lockPath := lockPathAt(root, "")
	lock, err := readLock(lockPath)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "go-purifier: warning: no go-purifier.lock.json found; running go impurely. Run `go-purifier lock` to use Nix-resolved dependencies.")
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	if lock.Version != lockFormatVersion {
		if changing {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("%s has lock format version %d and this go-purifier reads version %d; run `go-purifier lock` to write it again, or set GO_PURIFIER=0 to run go without it", lockPath, lock.Version, lockFormatVersion)
	}
	definition, err := definitionHash(root, lock.DefinitionFiles, toolLockOverrides())
	if err != nil {
		return nil, "", err
	}
	if lock.DefinitionHash != definition {
		if !changing {
			return nil, "", fmt.Errorf("go.mod, go.sum or go.work changed since %s was written; run `go-purifier lock`, or set GO_PURIFIER=0 to run go without the lockfile", lockPath)
		}
		if len(lock.ProjectInputs) == 0 {
			return nil, "", nil
		}
	}
	proxyDir, err := realize(root, lockPath)
	if err != nil {
		return nil, "", err
	}
	inputs, err := resolveModuleInputs(root, lock.ProjectInputs, nil)
	if err != nil {
		return nil, "", err
	}
	work, err := inputs.bind(root)
	if err != nil {
		return nil, "", err
	}
	var inputEnvironment []string
	if work != "" {
		inputEnvironment = append(inputEnvironment, "GOWORK="+work)
	}
	if changing {
		goproxy, err := goOutput(goBin, dir, os.Environ(), "env", "GOPROXY")
		if err != nil {
			return nil, "", err
		}
		return append(inputEnvironment, []string{
			"GOPROXY=" + moduleInputProxy(inputs.Repositories, "file://"+proxyDir+","+strings.TrimSpace(goproxy)),
			"GOTOOLCHAIN=local",
		}...), proxyDir, nil
	}
	return append(inputEnvironment, []string{
		"GOPROXY=" + moduleInputProxy(inputs.Repositories, "file://"+proxyDir),
		"GONOPROXY=" + noProxyMatchesNothing,
		// The lockfile pins every download by hash, and go.sum still holds
		// the module checksums; the checksum database is not reachable
		// offline.
		"GOSUMDB=off",
		"GOTOOLCHAIN=local",
	}...), proxyDir, nil
}

// stateDirName is the directory, at the lock root, that holds the GC roots
// of the locked downloads and the toolchain. Projects keep it out of
// version control.
const stateDirName = ".go-purifier"

// realize builds the proxy tree of a lockfile through Nix and returns the
// path of a GC root that links to it, in the state directory at the lock
// root. Nix fetches every locked download in parallel, makes the files of
// modules from version control, and links them into one tree in the store.
// A tree built from the same lockfile content is reused.
func realize(root, lockPath string) (string, error) {
	dir := filepath.Join(root, stateDirName)
	proxyLink := filepath.Join(dir, "proxy")
	stampPath := filepath.Join(dir, "stamp")

	content, err := os.ReadFile(lockPath)
	if err != nil {
		return "", err
	}
	contentSum := sha256.Sum256(content)
	stamp := hex.EncodeToString(contentSum[:])
	if old, err := os.ReadFile(stampPath); err == nil && string(old) == stamp {
		if _, err := os.Stat(proxyLink); err == nil {
			return proxyLink, nil
		}
	}

	nixLib, nixpkgs := os.Getenv("GO_PURIFIER_NIX_LIB"), os.Getenv("GO_PURIFIER_NIXPKGS")
	if nixLib == "" || nixpkgs == "" {
		return "", errors.New("GO_PURIFIER_NIX_LIB or GO_PURIFIER_NIXPKGS is not set; run go through the go-purifier flake (`nix develop` or `nix shell` of its `toolchain` package), or set GO_PURIFIER=0")
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "go-purifier: realizing %s through Nix\n", lockPath)
	args := []string{filepath.Join(nixLib, "realize.nix"),
		"--argstr", "lockFile", lockPath,
		"--argstr", "cli", self,
		"--argstr", "nixpkgs", nixpkgs,
		"--out-link", proxyLink, "--quiet"}
	// nix-build prints the store path of the tree on standard output, which
	// is the output of the go command the user ran.
	build := exec.Command("nix-build", args...)
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return "", err
	}
	if err := os.WriteFile(stampPath, []byte(stamp), 0o644); err != nil {
		return "", err
	}
	return proxyLink, nil
}

// linkTreeCommand is the builder of the proxy tree that interactive mode
// realizes. The file $manifestPath holds a line of a tree path, a tab, and
// a store path per entry; the builder makes $out a directory with a link
// to each store path at its tree path.
func linkTreeCommand() error {
	out, manifestPath := os.Getenv("out"), os.Getenv("manifestPath")
	if out == "" || manifestPath == "" {
		return errors.New("link-tree builds $out from $manifestPath, and one of them is not set")
	}
	f, err := os.Open(manifestPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		rel, target, ok := strings.Cut(scanner.Text(), "\t")
		if !ok {
			continue
		}
		link := filepath.Join(out, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(target, link); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if infos := os.Getenv("infosPath"); infos != "" {
		metadata, err := os.Open(infos)
		if err != nil {
			return err
		}
		defer metadata.Close()
		scanner := bufio.NewScanner(metadata)
		for scanner.Scan() {
			rel, data, ok := strings.Cut(scanner.Text(), "\t")
			if !ok {
				return fmt.Errorf("invalid proxy metadata manifest line")
			}
			target := filepath.Join(out, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(target, []byte(data+"\n"), 0o644); err != nil {
				return err
			}
		}
		return scanner.Err()
	}
	return nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func output(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}
