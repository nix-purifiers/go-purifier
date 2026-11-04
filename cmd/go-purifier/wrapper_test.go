// SPDX-License-Identifier: MIT

package main

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkingDir(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"build", "./..."}, cwd},
		{[]string{"-C", "sub", "build"}, filepath.Join(cwd, "sub")},
		{[]string{"-C=sub", "build"}, filepath.Join(cwd, "sub")},
		{[]string{"-C", "/abs/dir", "test"}, "/abs/dir"},
		{[]string{"build", "-C", "sub"}, cwd},
	}
	for _, c := range cases {
		got, err := workingDir(c.args)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("workingDir(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestFindLockRoot(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"app/go.mod":     "module example.com/app\n",
		"app/cmd/x/x.go": "package main\n",
	})
	deep := filepath.Join(root, "app", "cmd", "x")

	t.Setenv("GOWORK", "")
	if got, ok := findLockRoot(deep); !ok || got != filepath.Join(root, "app") {
		t.Errorf("findLockRoot of a module = %q, %v; want the module directory", got, ok)
	}

	writeFiles(t, root, map[string]string{"go.work": "go 1.24\n\nuse ./app\n"})
	if got, ok := findLockRoot(deep); !ok || got != root {
		t.Errorf("findLockRoot in a workspace = %q, %v; want the workspace directory", got, ok)
	}

	t.Setenv("GOWORK", "off")
	if got, ok := findLockRoot(deep); !ok || got != filepath.Join(root, "app") {
		t.Errorf("findLockRoot with GOWORK=off = %q, %v; want the module directory", got, ok)
	}
}

// A build without a lockfile, and a build whose go.mod changed since its
// lockfile was written, run go unchanged, without Nix.
func TestInteractiveEnvLeavesUnlockedBuildsAlone(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GOWORK", "off")
	writeFiles(t, root, map[string]string{"go.mod": "module example.com/m\n"})
	env, proxy, err := interactiveEnv("go", root, false)
	if err != nil || env != nil || proxy != "" {
		t.Fatalf("interactiveEnv without a lockfile = %v, %q, %v; want nothing", env, proxy, err)
	}

	stale := &Lock{Version: lockFormatVersion, DefinitionFiles: []string{"go.mod", "go.sum"}, DefinitionHash: "sha256-stale"}
	if err := writeLock(filepath.Join(root, lockFileName), stale); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GO_PURIFIER_NIX_LIB", "")
	// A build command stops with the command to run; a command that changes
	// the dependencies goes on the usual way.
	_, _, err = interactiveEnv("go", root, false)
	if err == nil || !strings.Contains(err.Error(), "run `go-purifier lock`") {
		t.Fatalf("interactiveEnv of a build command with a stale lockfile = %v; want an error that names go-purifier lock", err)
	}
	env, proxy, err = interactiveEnv("go", root, true)
	if err != nil || env != nil || proxy != "" {
		t.Fatalf("interactiveEnv of go get with a stale lockfile = %v, %q, %v; want nothing", env, proxy, err)
	}
}

// A lockfile changed after go-purifier wrote it fails with the command to
// run, and one written by writeLock reads back.
func TestContentHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), lockFileName)
	lock := &Lock{
		Version:   lockFormatVersion,
		Binaries:  []Binary{},
		Proxy:     "https://proxy.example/?a=b&c=<d>",
		Files:     map[string]string{"example.com/x/@v/v1.0.0.zip": "sha256-x"},
		Checkouts: []Artifact{},
	}
	if err := writeLock(path, lock); err != nil {
		t.Fatal(err)
	}
	if _, err := readLock(path); err != nil {
		t.Fatalf("readLock of a lockfile writeLock wrote: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), "sha256-x", "sha256-y", 1)
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readLock(path); err == nil || !strings.Contains(err.Error(), "go-purifier lock") {
		t.Fatalf("readLock of an edited lockfile = %v; want an error that names go-purifier lock", err)
	}
	// The digest is that of the JSON Nix's builtins.toJSON gives the same
	// value: sorted keys, no whitespace, no HTML escaping.
	got, err := contentHash([]byte(`{"b":[1,{"y":null,"x":"<&>"}],"a":true,"contentHash":"ignored"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := sri(sha256Sum(`{"a":true,"b":[1,{"x":"<&>","y":null}]}`))
	if got != want {
		t.Errorf("contentHash = %s, want %s", got, want)
	}
}

func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func TestLockfileFlag(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		args []string
		path string
		rest []string
	}{
		{[]string{"v1.0.0"}, "", []string{"v1.0.0"}},
		{[]string{"--lockfile", "/locks/x.json", "v1.0.0"}, "/locks/x.json", []string{"v1.0.0"}},
		{[]string{"--lockfile=locks/x.json"}, filepath.Join(cwd, "locks/x.json"), nil},
	}
	for _, c := range cases {
		path, rest, err := lockfileFlag(c.args)
		if err != nil || path != c.path || strings.Join(rest, " ") != strings.Join(c.rest, " ") {
			t.Errorf("lockfileFlag(%q) = %q, %q, %v; want %q, %q", c.args, path, rest, err, c.path, c.rest)
		}
	}
	if _, _, err := lockfileFlag([]string{"--lockfile"}); err == nil {
		t.Error("--lockfile without a path was accepted")
	}
	t.Setenv("GO_PURIFIER_LOCKFILE", "/elsewhere/go-purifier.lock.json")
	if got := lockPathAt("/src", ""); got != "/elsewhere/go-purifier.lock.json" {
		t.Errorf("lockPathAt with GO_PURIFIER_LOCKFILE = %q", got)
	}
	if got := lockPathAt("/src", "/flag.json"); got != "/flag.json" {
		t.Errorf("lockPathAt with a flag = %q", got)
	}
}

func TestStoreObject(t *testing.T) {
	if got, ok := storeObject("/nix/store/abc-go-1.26.10/bin/go"); !ok || got != "/nix/store/abc-go-1.26.10" {
		t.Errorf("storeObject of a store file = %q, %v", got, ok)
	}
	if _, ok := storeObject("/usr/bin/go"); ok {
		t.Error("storeObject accepted a path outside the store")
	}
}

// A lockfile that is not JSON gets an error that names the command that
// writes it again.
func TestInteractiveEnvRejectsMalformedLockfile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GOWORK", "off")
	writeFiles(t, root, map[string]string{"go.mod": "module example.com/m\n", lockFileName: "{ not json"})
	_, _, err := interactiveEnv("go", root, false)
	if err == nil || !strings.Contains(err.Error(), "go-purifier lock") {
		t.Fatalf("interactiveEnv with a malformed lockfile = %v; want an error that names go-purifier lock", err)
	}
}

func TestChangesDependencies(t *testing.T) {
	cases := map[string]bool{
		"build ./...":                       false,
		"test ./...":                        false,
		"run .":                             false,
		"vet ./...":                         false,
		"get example.com/x@v1.2.3":          true,
		"get -u ./...":                      true,
		"-C sub get example.com/x":          true,
		"mod tidy":                          true,
		"mod download":                      true,
		"mod graph":                         false,
		"install example.com/cmd/x@latest":  true,
		"install ./cmd/x":                   false,
		"run example.com/cmd/x@v1.0.0 -arg": true,
		"list -m -u all":                    true,
		"list -m all":                       false,
		"work sync":                         true,
		"":                                  false,
	}
	for line, want := range cases {
		if got := changesDependencies(strings.Fields(line)); got != want {
			t.Errorf("changesDependencies(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestTailBuffer(t *testing.T) {
	var b tailBuffer
	b.Write([]byte(strings.Repeat("a", 70<<10)))
	b.Write([]byte("end"))
	if len(b.data) != 64<<10 || !strings.HasSuffix(b.String(), "end") {
		t.Errorf("tailBuffer kept %d bytes ending %q", len(b.data), b.String()[len(b.data)-3:])
	}
}

func TestLinkTree(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest")
	writeFiles(t, dir, map[string]string{"manifest": "a/@v/v1.0.0.zip\t/nix/store/x-a.zip\nb/c/@v/v2.0.0.mod\t/nix/store/y-c.mod\n"})
	out := filepath.Join(dir, "out")
	t.Setenv("out", out)
	t.Setenv("manifestPath", manifest)
	infos := filepath.Join(dir, "infos")
	writeFiles(t, dir, map[string]string{"infos": "b/c/@v/v2.0.0.info\t{\"Version\":\"v2.0.0\"}\n"})
	t.Setenv("infosPath", infos)
	if err := linkTreeCommand(); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{"a/@v/v1.0.0.zip": "/nix/store/x-a.zip", "b/c/@v/v2.0.0.mod": "/nix/store/y-c.mod"} {
		got, err := os.Readlink(filepath.Join(out, link))
		if err != nil || got != target {
			t.Errorf("link %s = %q, %v; want %q", link, got, err, target)
		}
	}
	payload, err := os.ReadFile(filepath.Join(out, "b/c/@v/v2.0.0.info"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(payload, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["Version"] != "v2.0.0" || metadata["Time"] != nil {
		t.Fatalf("version metadata must not invent an upstream timestamp: %s", payload)
	}
}
