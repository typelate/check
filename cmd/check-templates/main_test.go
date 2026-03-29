package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rsc.io/script"
	"rsc.io/script/scripttest"
)

func Test(t *testing.T) {
	e := script.NewEngine()
	e.Quiet = true
	e.Cmds = scripttest.DefaultCmds()
	e.Cmds["check-templates"] = checkTemplatesCommand()
	ctx := t.Context()
	scripttest.Test(t, ctx, e, nil, filepath.FromSlash("testdata/*.txt"))
}

// fixtureSpec is the schema for testdata/fixtures/<name>/fixture.json.
type fixtureSpec struct {
	Flags        []string `json:"flags"`
	WantExitCode int      `json:"wantExitCode"`
	// WantStderr lists substrings that must each appear in stderr output.
	// Use forward slashes; they are normalised to the OS path separator.
	WantStderr []string `json:"wantStderr"`
}

// TestFixtures runs check-templates against real Go + template files under
// testdata/fixtures/<name>/ and verifies exit code and stderr output.
func TestFixtures(t *testing.T) {
	fixturesDir := filepath.Join("testdata", "fixtures")
	entries, err := os.ReadDir(fixturesDir)
	if err != nil {
		t.Fatalf("reading fixtures dir: %v", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		dir := filepath.Join(fixturesDir, name)

		t.Run(name, func(t *testing.T) {
			specPath := filepath.Join(dir, "fixture.json")
			data, err := os.ReadFile(specPath)
			if err != nil {
				t.Fatalf("reading fixture.json: %v", err)
			}
			var spec fixtureSpec
			if err := json.Unmarshal(data, &spec); err != nil {
				t.Fatalf("parsing fixture.json: %v", err)
			}

			absDir, err := filepath.Abs(dir)
			if err != nil {
				t.Fatalf("abs path: %v", err)
			}

			var stdout, stderr bytes.Buffer
			args := append(spec.Flags, "./...")
			gotCode := run(absDir, args, &stdout, &stderr)

			if gotCode != spec.WantExitCode {
				t.Errorf("exit code: got %d, want %d\nstderr:\n%s", gotCode, spec.WantExitCode, stderr.String())
			}

			stderrStr := filepath.ToSlash(stderr.String())
			for _, want := range spec.WantStderr {
				if !strings.Contains(stderrStr, want) {
					t.Errorf("stderr missing %q\nfull stderr:\n%s", want, stderrStr)
				}
			}
		})
	}
}

func checkTemplatesCommand() script.Cmd {
	return script.Command(script.CmdUsage{
		Summary: "check-templates [dir]",
		Args:    "[dir]",
	}, func(state *script.State, args ...string) (script.WaitFunc, error) {
		return func(state *script.State) (string, string, error) {
			var stdout, stderr bytes.Buffer
			code := run(state.Getwd(), args, &stdout, &stderr)
			var err error
			if code != 0 {
				err = script.ErrUsage
			}
			return stdout.String(), stderr.String(), err
		}, nil
	})
}
