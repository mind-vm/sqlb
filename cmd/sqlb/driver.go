package main

// The mechanics of compiling a program against someone else's schema package.
//
// Three steps, each of which can fail in a way worth a different message: find
// the package and the module that holds it, check it declares the function this
// command needs before spending a compile on finding out, and build and run the
// driver from a temporary directory.

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/mind-vm/sqlb/codegen"
)

// funcSignature is what the usage text and every error message say a schema
// package must export. Built from the constant the driver actually calls, so
// the documentation cannot drift from the convention.
const funcSignature = codegen.ProjectFunc + "() codegen.Project"

// pkg is the part of `go list` output this command uses.
type pkg struct {
	ImportPath string
	Name       string
	Dir        string
	Module     struct {
		Path string
		Dir  string
	}
}

// resolve turns a package pattern into the one package it names, and refuses
// anything else.
//
// A pattern is accepted rather than an import path because that is what every
// other Go command takes and what a //go:generate directive can write relative
// to itself. `./...` matching four packages is refused rather than guessed at:
// generating from the wrong registry produces plausible Go describing the wrong
// tables, which is worse than an error.
func resolve(pattern string) (*pkg, error) {
	cmd := exec.Command("go", "list", "-json", "--", pattern)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("could not resolve the package %q:\n%s", pattern, indent(msg))
	}

	// go list -json emits one object per package, concatenated rather than
	// wrapped in an array, so this decodes in a loop.
	var found []*pkg
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		p := new(pkg)
		if err := dec.Decode(p); err != nil {
			return nil, fmt.Errorf("could not read go list output for %q: %w", pattern, err)
		}
		found = append(found, p)
	}

	switch {
	case len(found) == 0:
		return nil, fmt.Errorf("the pattern %q matched no packages", pattern)
	case len(found) > 1:
		var names []string
		for _, p := range found {
			names = append(names, p.ImportPath)
		}
		return nil, fmt.Errorf(
			"the pattern %q matched %d packages, and sqlb needs exactly one — "+
				"the registry it reads is whichever ones got linked in, so this "+
				"is refused rather than guessed:\n%s",
			pattern, len(found), indent(strings.Join(names, "\n")))
	}

	p := found[0]
	if p.Name == "main" {
		return nil, fmt.Errorf(
			"%s is package main, which cannot be imported. Point sqlb at the package "+
				"that declares the schema, not at the command that used to generate from it",
			p.ImportPath)
	}
	if p.Module.Dir == "" {
		return nil, fmt.Errorf(
			"%s is not in a module, and sqlb resolves every output path against the "+
				"directory holding go.mod", p.ImportPath)
	}
	return p, nil
}

// declaresProject reports whether the package exports the convention function,
// so that a missing one is a sentence rather than a compile error inside a
// temporary file the user never sees.
//
// Build constraints are ignored here. This is a guard whose job is a better
// message, not a second type checker — the compiler in the next step is what
// actually decides, and it will disagree with this only for a schema package
// that hides its declaration behind a build tag.
func declaresProject(p *pkg) (bool, error) {
	entries, err := os.ReadDir(p.Dir)
	if err != nil {
		return false, fmt.Errorf("could not read %s: %w", p.Dir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, 0)
		if err != nil {
			// A file that does not parse is the compiler's problem to report,
			// with a position and a caret. Failing here instead would replace
			// that with a worse message about a guard.
			continue
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != codegen.ProjectFunc {
				continue
			}
			if len(fn.Type.Params.List) != 0 {
				return false, fmt.Errorf(
					"%s.%s takes arguments; sqlb calls it with none. It must be: func %s",
					p.Name, codegen.ProjectFunc, funcSignature)
			}
			return true, nil
		}
	}
	return false, nil
}

// driverSource is the program cmd/sqlb compiles inside the target module.
//
// It is three lines because everything it could otherwise contain is in
// codegen.Main, where it can be tested. Generated code that is never written to
// disk in a form anyone reviews should do as little as it can get away with.
const driverSource = `// Code generated by sqlb. DO NOT EDIT.
//
// Written to a temporary directory INSIDE the target module, compiled, run and
// deleted. It exists because a schema is registered by importing the package
// that declares it, so reading one requires linking against it.
package main

import (
	"github.com/mind-vm/sqlb/codegen"

	schemapkg %q
)

func main() { codegen.Main(schemapkg.%s()) }
`

// driverPrefix names the scratch directory the driver is compiled in. It is one
// constant rather than a literal in three places because .gitignore has a rule
// spelling it out, and the sweep below has to agree with what MkdirTemp wrote.
const driverPrefix = ".sqlb-driver-"

// staleAfter is how old an abandoned scratch directory must be before a later
// run removes it. Two sqlb runs in one module share the directory the sweep
// walks, so this is set far beyond any plausible cold compile: deleting a live
// run's driver out from under it would produce a failure nobody could explain.
const staleAfter = time.Hour

// newScratch creates the directory the driver is written to and returns the
// function that removes it.
//
// The removal is wired to signals as well as to the returned function, because
// the compile in the middle of drive is the longest thing this command does and
// therefore the likeliest moment for someone to lose patience with it. A
// deferred RemoveAll does not run when the process is killed by SIGINT, so
// without this a Ctrl-C during `sqlb generate` leaves a directory behind in a
// tree the user is about to `git add`. That is not hypothetical: one of them
// reached a commit and had to be caught by hand.
func newScratch(moduleDir string, stderr io.Writer) (string, func(), error) {
	tmp, err := os.MkdirTemp(moduleDir, driverPrefix)
	if err != nil {
		return "", nil, err
	}

	// Only signals that would otherwise kill this process. A shell starting a
	// command in the background sets SIGINT to ignored in the child, and
	// signal.Notify would quietly undo that — turning `sqlb generate &` in a
	// script from something a Ctrl-C leaves running into something it kills.
	// Nothing is lost by respecting it: a signal that is ignored does not end
	// the process, so the deferred cleanup still runs.
	var want []os.Signal
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		if !signal.Ignored(sig) {
			want = append(want, sig)
		}
	}

	sigs := make(chan os.Signal, 1)
	// Guarded, because signal.Notify with an empty list means the opposite of
	// what the loop above computed: it relays *every* signal.
	if len(want) > 0 {
		signal.Notify(sigs, want...)
	}
	go func() {
		sig, ok := <-sigs
		if !ok {
			// The channel was closed by the cleanup below: drive finished
			// on its own and there is nothing left to remove.
			return
		}
		removeScratch(tmp, stderr)
		// Exit the way a shell expects a signalled process to, rather than
		// with a bare 1. `sqlb check` in a script reports staleness with an
		// exit code, and an interrupted run must stay distinguishable from
		// a run that found something.
		code := 1
		if s, ok := sig.(syscall.Signal); ok {
			code = 128 + int(s)
		}
		os.Exit(code)
	}()

	return tmp, func() {
		// Stop before closing: signal.Stop guarantees nothing further is
		// sent on the channel once it returns, which is what makes the
		// close safe rather than a race with a delivery.
		signal.Stop(sigs)
		close(sigs)
		removeScratch(tmp, stderr)
	}, nil
}

// removeScratch deletes a scratch directory, retries once, and says so if it
// still cannot.
//
// The retry is for macOS, where a removal racing concurrent filesystem activity
// fails transiently and succeeds a moment later. The report is the part that
// matters: the discarded error was the reason a leaked directory was both
// invisible and unattributable, and it failed the very test that polices
// leaking (#78). A failure here is not worth ending the run over — the work the
// user asked for is done by the time this runs — but it must not be silent.
func removeScratch(dir string, stderr io.Writer) {
	err := os.RemoveAll(dir)
	if err == nil {
		return
	}
	time.Sleep(50 * time.Millisecond)
	if err = os.RemoveAll(dir); err == nil {
		return
	}
	if stderr != nil {
		_, _ = fmt.Fprintf(stderr, "sqlb: could not remove the scratch directory %s: %v\n", dir, err)
	}
}

// sweepScratch removes scratch directories abandoned by earlier runs.
//
// newScratch covers every exit this process can observe. It cannot cover the
// ones it cannot: SIGKILL, a panic in the runtime, a laptop that loses power
// mid-compile, or the narrow window where `go build` is still writing into the
// directory as the signal handler removes it. Those leave a .sqlb-driver-* in
// the user's tree that nothing else would ever clean up, so the next run does.
//
// Best effort throughout. A leftover that cannot be read or removed — a
// permissions problem, or another user's — is not a reason to refuse to
// generate, and saying so would be a message about housekeeping in front of the
// work someone actually asked for.
func sweepScratch(moduleDir string) {
	entries, err := os.ReadDir(moduleDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), driverPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < staleAfter {
			continue
		}
		_ = os.RemoveAll(filepath.Join(moduleDir, e.Name()))
	}
}

// drive builds the driver and runs it with the given verb and flags.
//
// Build and run are separate steps rather than one `go run` so that the child's
// exit code arrives here unmodified. `go run` reports a non-zero exit by
// printing "exit status 1" of its own, which on a `sqlb check` failure lands
// underneath the list of stale files and reads like a second, unexplained
// error.
func drive(p *pkg, args []string, stdout, stderr io.Writer) error {
	ok, err := declaresProject(p)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf(
			"%s does not export %s.\n\nsqlb reads a project's output directories from that "+
				"function, because they are Go rather than a config file that could disagree "+
				"with the type. Add one to %s:\n\n"+
				"    func %s {\n"+
				"        return codegen.Project{\n"+
				"            Options: codegen.Options{Package: %q},\n"+
				"        }\n"+
				"    }",
			p.ImportPath, codegen.ProjectFunc,
			filepath.Join(p.Dir, "sqlb.go"), funcSignature, p.Name)
	}

	// Inside the module, not in the system temp directory, and the difference
	// decides whether this command works at all on a large repository.
	//
	// Go refuses to import a package under internal/ from outside the tree
	// rooted at that internal/'s parent. A driver in /tmp is outside every
	// module, so a schema package at internal/…/fooschema produced
	//
	//	use of internal package … not allowed
	//
	// and no amount of build.Dir helped: the rule is about where the importing
	// FILE lives. Repositories that group their modules under internal/ — which
	// is the recommended layout for anything with a public surface worth
	// protecting — could therefore not use this command for the thing it exists
	// to do, and had to keep the hand-written cmd/gen/main.go ADR-0032 removed.
	//
	// The directory is dot-prefixed so that the go tool ignores it if a build
	// races with one that is still on disk. Removing it is newScratch's job,
	// which is more than a defer because it is somebody's working tree.
	sweepScratch(p.Module.Dir)
	tmp, done, err := newScratch(p.Module.Dir, stderr)
	if err != nil {
		return fmt.Errorf(
			"could not create a temporary directory in %s, which is where the driver has "+
				"to be written: Go refuses an internal/ import from a file outside the "+
				"module, so a schema package under internal/ cannot be read from anywhere "+
				"else: %w", p.Module.Dir, err)
	}
	defer done()

	src := filepath.Join(tmp, "main.go")
	body := fmt.Sprintf(driverSource, p.ImportPath, codegen.ProjectFunc)
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		return err
	}

	bin := filepath.Join(tmp, "driver")
	build := exec.Command("go", "build", "-o", bin, src)
	// The module root, not the working directory: this is what makes every
	// path in a Project mean the same thing wherever the command was invoked
	// from, which is the whole reason the old generators needed -dir.
	build.Dir = p.Module.Dir
	var buildErr strings.Builder
	build.Stderr = &buildErr
	if err := build.Run(); err != nil {
		return fmt.Errorf(
			"the generated driver did not compile against %s:\n%s\n"+
				"The driver is three lines, so this is almost always %s itself or "+
				"something it imports",
			p.Module.Path, indent(strings.TrimSpace(buildErr.String())), p.ImportPath)
	}

	cmd := exec.Command(bin, args...)
	cmd.Dir = p.Module.Dir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return exitCode(ee.ExitCode())
		}
		return err
	}
	return nil
}

// version reports what this binary was built from, which for a tool whose whole
// job is keeping generated files in step with a library matters more than
// usual: a `sqlb check` failure that nobody can reproduce is nearly always two
// machines running two versions.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "sqlb (unknown version: no build info)"
	}
	v := info.Main.Version
	if v == "" || v == "(devel)" {
		// Built from a checkout rather than installed from a tag. The VCS
		// stamp is the only thing that identifies it, and it is absent when
		// the tree was built from an archive rather than a git clone.
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return "sqlb (devel " + short(s.Value) + ")"
			}
		}
		return "sqlb (devel)"
	}
	return "sqlb " + v
}

func short(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

// indent offsets a block quoted inside an error message, so that a multi-line
// compiler error reads as one quoted thing rather than as several errors.
func indent(s string) string {
	if s == "" {
		return s
	}
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}
