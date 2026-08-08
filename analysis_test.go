package check_test

import (
	"os"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
	"rsc.io/script"
	"rsc.io/script/scripttest"

	"github.com/typelate/check"
)

func TestScript(t *testing.T) {
	engine := &script.Engine{
		Cmds:  scripttest.DefaultCmds(),
		Conds: scripttest.DefaultConds(),
	}

	// Add our analyzer command
	engine.Cmds["check"] = script.Command(
		script.CmdUsage{
			Summary: "run check analyzer",
			Args:    "patterns...",
		},
		func(s *script.State, args ...string) (script.WaitFunc, error) {
			if len(args) == 0 {
				args = []string{"."}
			}

			// Run analyzer using analysistest machinery
			dir := s.Getwd()
			results := analysistest.Run(t, dir, check.Analyzer, args...)

			// Collect diagnostics
			for _, r := range results {
				for _, d := range r.Diagnostics {
					pos := r.Pass.Fset.Position(d.Pos)
					s.Logf("%s:%d: %s", pos.Filename, pos.Line, d.Message)
				}
			}

			return nil, nil
		},
	)

	env := os.Environ()

	scripttest.Test(t, t.Context(), engine, env, "testdata/*.txt")
}
