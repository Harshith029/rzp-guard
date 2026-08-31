// Command redteam-audit enforces the red-team lane's invariants structurally,
// from inside the lane.
//
// WHY THIS EXISTS.
//
// The negative suite's "no configurable child" test was a shell deny-list:
// it grepped for `exec.Command("sh"` and `Getenv(...CHILD`. Review pointed out
// that this is exactly the rename/evasion class it claimed to solve --
// `exec.CommandContext(ctx, "sh", "-c", …)`, `os.LookupEnv`, a flag, or a
// constructed variable name all walk straight past it.
//
// A deny-list of spellings cannot express "there is no way to configure the
// child". A POSITIVE STRUCTURAL assertion can: across every file the redteam
// build actually compiles there is exactly ONE process launch, it is
// exec.CommandContext(ctx, redteamChildPath), and it takes no further
// arguments. Anything else -- a second launch, an extra argument, a shell, a
// path read from anywhere -- fails, whatever it is named.
//
// It also runs INSIDE the isolated lane. The previous version shelled out to
// `gorun`, which mounts the real working tree including .env with networking
// on -- so the command reviewers were told to trust as evidence was itself the
// bypass it existed to detect. Third occurrence of that mistake; this one is
// structural rather than a promise not to repeat it.
package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: redteam-audit child <file...> | stub <response-file>")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "child":
		err = auditChild(os.Args[2:])
	case "stub":
		err = auditStubResponse(os.Args[2])
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "redteam-audit: %v\n", err)
		os.Exit(1)
	}
}

// launch is one process-spawning call found in the source.
type launch struct {
	pos      token.Position
	callee   string
	argCount int
	argsDesc string
}

// auditChild asserts the positive structure of the redteam build's only child.
func auditChild(files []string) error {
	if len(files) == 0 {
		return fmt.Errorf("no source files given; the caller must pass exactly " +
			"the files `go list -tags redteam` reports, or this audits nothing")
	}

	fset := token.NewFileSet()
	var launches []launch
	var configReads []token.Position
	var problems []string
	sawChildFile := false

	for _, f := range files {
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", f, err)
		}
		isChildFile := strings.HasSuffix(filepath.Base(f), "child_redteam.go")
		if isChildFile {
			sawChildFile = true
		}

		// Map each file's LOCAL import names to their package paths, so a call
		// is identified by what it actually resolves to.
		//
		// This audit previously matched the textual selector: `exec.Command…`.
		// Review pointed out that `import runner "os/exec"` then
		// `runner.CommandContext(ctx, "sh", "-c", …)` is invisible to that, and
		// a demonstration confirmed it -- the audit printed "exactly one launch,
		// no configuration reads" and exited 0 with a hidden shell launch in the
		// file. A positive structural claim that a rename defeats is not a
		// structural claim.
		imports := map[string]string{}
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			switch {
			case imp.Name == nil:
				parts := strings.Split(path, "/")
				imports[parts[len(parts)-1]] = path
			case imp.Name.Name == ".":
				// A dot import makes os/exec calls appear bare and unqualified.
				// Rather than try to resolve that, refuse it: there is no reason
				// for one here, and permitting it would be a hole by design.
				problems = append(problems, fmt.Sprintf(
					"%s: dot-import of %q; calls become unqualified and cannot be "+
						"attributed, so it is refused in these files",
					fset.Position(imp.Pos()), path))
			case imp.Name.Name == "_":
				// Blank: cannot be called through.
			default:
				imports[imp.Name.Name] = path
			}
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := resolvedCallee(call.Fun, imports)

			// Every way a Go program starts a process.
			switch name {
			case "os/exec.Command", "os/exec.CommandContext",
				"syscall.Exec", "syscall.ForkExec", "syscall.StartProcess",
				"os.StartProcess":
				launches = append(launches, launch{
					pos: fset.Position(call.Pos()), callee: name,
					argCount: len(call.Args), argsDesc: describeArgs(call.Args),
				})
			}

			// Reading the child from ANY configuration source, inside the file
			// that defines the child. envWithoutRazorpayKeys legitimately calls
			// os.Environ in main.go, so this is scoped to the child file.
			if isChildFile {
				switch name {
				case "os.Getenv", "os.LookupEnv", "os.Environ",
					"flag.String", "flag.Bool", "flag.Lookup",
					"flag.StringVar", "flag.Parse":
					configReads = append(configReads, fset.Position(call.Pos()))
				}
			}
			return true
		})
	}

	if !sawChildFile {
		return fmt.Errorf("child_redteam.go is not among the compiled files; the " +
			"redteam build is not selecting the child it is supposed to")
	}

	if len(launches) != 1 {
		problems = append(problems, fmt.Sprintf(
			"the redteam build contains %d process launches, want exactly 1", len(launches)))
		for _, l := range launches {
			problems = append(problems, fmt.Sprintf("    %s: %s(%s)",
				l.pos, l.callee, l.argsDesc))
		}
	} else {
		l := launches[0]
		if !strings.HasSuffix(filepath.Base(l.pos.Filename), "child_redteam.go") {
			problems = append(problems, fmt.Sprintf(
				"the only process launch is in %s, want child_redteam.go", l.pos.Filename))
		}
		if l.callee != "os/exec.CommandContext" {
			problems = append(problems, fmt.Sprintf(
				"%s: launch is %s, want exec.CommandContext so the child is "+
					"cancellable with the session", l.pos, l.callee))
		}
		// ctx + path, and nothing else. An extra argument is how "-c" returns.
		if l.argCount != 2 {
			problems = append(problems, fmt.Sprintf(
				"%s: launch takes %d arguments (%s), want exactly 2 (ctx, path) -- "+
					"a third argument is how a shell flag comes back",
				l.pos, l.argCount, l.argsDesc))
		}
		if l.argCount == 2 && !strings.HasSuffix(l.argsDesc, "redteamChildPath") {
			problems = append(problems, fmt.Sprintf(
				"%s: launches %s, want the constant redteamChildPath -- a variable "+
					"here means the path came from somewhere", l.pos, l.argsDesc))
		}
	}

	for _, p := range configReads {
		problems = append(problems, fmt.Sprintf(
			"%s: child_redteam.go reads configuration; the child must not be "+
				"selectable at runtime by any name", p))
	}

	if len(problems) > 0 {
		return fmt.Errorf("redteam child structure violated:\n  %s",
			strings.Join(problems, "\n  "))
	}
	fmt.Printf("redteam child: exactly one launch, exec.CommandContext(ctx, redteamChildPath), "+
		"no configuration reads (%d files audited)\n", len(files))
	return nil
}

// resolvedCallee names a call by the IMPORT PATH it resolves to, so
// `runner.CommandContext` with `import runner "os/exec"` reads as
// "os/exec.CommandContext" -- the same as the unaliased spelling.
func resolvedCallee(fun ast.Expr, imports map[string]string) string {
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		id, ok := f.X.(*ast.Ident)
		if !ok {
			return f.Sel.Name
		}
		if path, ok := imports[id.Name]; ok {
			return path + "." + f.Sel.Name
		}
		// Not an imported package: a local variable or receiver.
		return id.Name + "." + f.Sel.Name
	case *ast.Ident:
		return f.Name
	}
	return ""
}

func describeArgs(args []ast.Expr) string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		switch v := a.(type) {
		case *ast.Ident:
			out = append(out, v.Name)
		case *ast.BasicLit:
			out = append(out, v.Value)
		default:
			out = append(out, fmt.Sprintf("%T", a))
		}
	}
	return strings.Join(out, ", ")
}

// auditStubResponse parses a stub reply and requires a REAL tool error.
//
// The previous check was `case "$out" in *'"isError":true'*`, which passes if
// that text appears anywhere -- including inside ordinary tool content, which
// is a JSON string the stub itself composes. Parsed here, and the id is checked
// too, so the assertion is about the response to the request that was sent.
func auditStubResponse(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var seen int
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Result  *struct {
				IsError bool `json:"isError"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			return fmt.Errorf("stub emitted unparseable JSON: %w\n  %s", err, line)
		}
		seen++
		if msg.JSONRPC != "2.0" {
			return fmt.Errorf("stub reply is not JSON-RPC 2.0: %s", line)
		}
		if strings.TrimSpace(string(msg.ID)) != "1" {
			return fmt.Errorf("stub replied to id %s, want the id that was sent (1)", msg.ID)
		}
		if msg.Result == nil {
			return fmt.Errorf("stub reply carries no result: %s", line)
		}
		if !msg.Result.IsError {
			return fmt.Errorf("stub ACCEPTED a payment id outside its fixture set: "+
				"result.isError is false, so an invented id produced a normal "+
				"success\n  %s", line)
		}
	}
	if seen != 1 {
		return fmt.Errorf("expected exactly 1 stub reply, saw %d", seen)
	}
	fmt.Println("stub: refused a non-fixture id with result.isError == true (parsed, not matched)")
	return nil
}
