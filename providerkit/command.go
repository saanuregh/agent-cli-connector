package providerkit

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
)

// commandOutput runs a short-lived command, such as "--version", through
// spawner (nil means agent.LocalSpawner) and returns its trimmed stdout. ctx
// bounds the whole run; the process is killed when it ends.
func commandOutput(ctx context.Context, spawner agent.Spawner, cmd agent.Command) (string, error) {
	if spawner == nil {
		spawner = agent.LocalSpawner
	}
	proc, err := spawner.Spawn(ctx, cmd)
	if err != nil {
		return "", err
	}
	_ = proc.Stdin().Close()
	var stdout, stderr bytes.Buffer
	var wg sync.WaitGroup
	wg.Go(func() { _, _ = io.Copy(&stdout, proc.Stdout()) })
	wg.Go(func() { _, _ = io.Copy(&stderr, proc.Stderr()) })
	stop := context.AfterFunc(ctx, func() { _ = proc.Kill() })
	wg.Wait()
	err = proc.Wait()
	stop()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

// VersionStatus runs "<path> --version" and returns a ProviderStatus with
// Installed and Version filled. A command that cannot run yields
// Installed false and Err set. The error is non-nil only when ctx ended.
func VersionStatus(ctx context.Context, spawner agent.Spawner, path string, env []string) (*agent.ProviderStatus, error) {
	out, err := commandOutput(ctx, spawner, agent.Command{Path: path, Args: []string{"--version"}, Env: env})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return &agent.ProviderStatus{Err: err}, nil
	}
	return &agent.ProviderStatus{Installed: true, Version: providerutil.ParseVersion(out)}, nil
}

// CheckFailed records err, from a check step after VersionStatus, in st.Err
// and returns st. When ctx ended it returns ctx's error instead, because
// the check could not run rather than the harness failing it.
func CheckFailed(ctx context.Context, st *agent.ProviderStatus, err error) (*agent.ProviderStatus, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	st.Err = err
	return st, nil
}
