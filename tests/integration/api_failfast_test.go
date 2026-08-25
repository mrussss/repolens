package integration

import (
	"context"
	"net"
	"os/exec"
	"testing"
	"time"
)

func TestAPI_FailFastOnPortCollision(t *testing.T) {
	// Build the API binary
	cmdBuild := exec.Command("go", "build", "-o", "../../bin/repolens-api", "../../cmd/api/main.go")
	if err := cmdBuild.Run(); err != nil {
		t.Fatalf("failed to build API binary: %v", err)
	}

	// Occupy port 8099
	l, err := net.Listen("tcp", ":8099")
	if err != nil {
		t.Fatalf("failed to listen on port 8099: %v", err)
	}
	defer l.Close()

	// Run API with HTTP_PORT=8099
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmdRun := exec.CommandContext(ctx, "../../bin/repolens-api")
	cmdRun.Env = append(cmdRun.Env, "HTTP_PORT=8099", "ENV=testing")
	
	// Since port is occupied, ListenAndServe will fail immediately and the process should exit with code 1
	err = cmdRun.Run()
	if err == nil {
		t.Fatal("expected API server to fail-fast with error, but it exited cleanly")
	}
	
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatal("API server did not fail-fast; it hung until context timeout")
	}

	if exitErr, ok := err.(*exec.ExitError); ok {
		if exitErr.ExitCode() != 1 {
			t.Fatalf("expected exit code 1, got %d", exitErr.ExitCode())
		}
	} else {
		t.Fatalf("expected ExitError, got %v", err)
	}
}
