package main

import (
	"bytes"
	"path/filepath"
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
