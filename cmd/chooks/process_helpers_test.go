package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/nighthint"
	"github.com/aiseeq/chooks/internal/skillreload"
)

func processTestContext(t *testing.T) context.Context {
	t.Helper()
	request, err := hookctx.FromProcess()
	if err != nil {
		t.Fatal(err)
	}
	return hookctx.WithRequest(context.Background(), request)
}

func newHookTimer(hook string) *hookTimer {
	request, _ := hookctx.FromProcess()
	return newHookTimerForRequest(hook, request)
}

func processStart() (time.Time, error) {
	request, _ := hookctx.FromProcess()
	return processStartPID(request.PID)
}

func writeAllowedOutput(input *core.ToolInput, config *core.Config, logger core.Logger, out io.Writer) {
	request, _ := hookctx.FromProcess()
	writeAllowedOutputContext(hookctx.WithRequest(context.Background(), request), input, config, logger, out)
}

func runSessionEvent(eventName string, data []byte, config *core.Config, logger core.Logger) {
	request, _ := hookctx.FromProcess()
	runSessionEventForRequest(eventName, data, config, logger, &request)
}

func nightHintDeps(config *core.Config, deadline time.Time) (nighthint.Deps, error) {
	request, err := hookctx.FromProcess()
	if err != nil {
		return nighthint.Deps{}, err
	}
	return nightHintDepsForRequest(config, deadline, request)
}

func autonomousSkillCalled(config *core.Config, deadline time.Time) func(string) (bool, error) {
	return autonomousSkillCalledIn(config, deadline, func() (string, error) {
		request, err := hookctx.FromProcess()
		if err != nil {
			return "", err
		}
		return skillreload.StateDir(request)
	})
}

func skillReloadTestDir(t *testing.T) (string, error) {
	t.Helper()
	request, err := hookctx.FromProcess()
	if err != nil {
		return "", err
	}
	return skillreload.StateDir(request)
}
