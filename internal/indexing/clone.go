package indexing

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"repolens/internal/platform/logger"
)

var (
	privateIPBlocks []*net.IPNet
)

func init() {
	for _, cidr := range []string{
		"127.0.0.0/8",    // Loopback
		"10.0.0.0/8",     // RFC1918
		"172.16.0.0/12",  // RFC1918
		"192.168.0.0/16", // RFC1918
		"169.254.0.0/16", // Link-local / Cloud metadata
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // IPv6 ULA
		"fe80::/10",      // IPv6 link-local
	} {
		_, block, err := net.ParseCIDR(cidr)
		if err == nil {
			privateIPBlocks = append(privateIPBlocks, block)
		}
	}
}

func isPrivateOrLocalIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	for _, block := range privateIPBlocks {
		if block.Contains(ip) {
			return true
		}
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate()
}

type SafeGitCloner struct {
	allowHosts   []string
	maxSizeMB    int64
	cloneTimeout time.Duration
}

func NewSafeGitCloner(allowHosts []string, maxSizeMB int64, timeout time.Duration) *SafeGitCloner {
	if len(allowHosts) == 0 {
		allowHosts = []string{"github.com"}
	}
	if maxSizeMB <= 0 {
		maxSizeMB = 50
	}
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	return &SafeGitCloner{
		allowHosts:   allowHosts,
		maxSizeMB:    maxSizeMB,
		cloneTimeout: timeout,
	}
}

// ResolveRef resolves a ref before a Snapshot row is created. This makes the
// natural identity repository_id + exact commit_sha available to the API and
// avoids concurrent requests colliding on the placeholder "pending" value.
func (c *SafeGitCloner) ResolveRef(ctx context.Context, gitURL, ref string) (string, error) {
	if err := c.ValidateGitURL(gitURL); err != nil {
		return "", err
	}
	resolveCtx, cancel := context.WithTimeout(ctx, c.cloneTimeout)
	defer cancel()
	args := []string{"ls-remote", gitURL}
	if ref != "" && !isFullCommitSHA(ref) {
		args = append(args, ref, ref+"^{}", "refs/heads/"+ref, "refs/tags/"+ref, "refs/tags/"+ref+"^{}")
	}
	cmd := exec.CommandContext(resolveCtx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(resolveCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("git ref resolution timed out after %v", c.cloneTimeout)
		}
		return "", fmt.Errorf("git ref resolution failed: %w", err)
	}
	commitSHA := resolveCommitFromLsRemote(string(out), ref)
	if commitSHA == "" {
		return "", fmt.Errorf("git ref %q did not resolve to an exact commit SHA", ref)
	}
	return commitSHA, nil
}

func isFullCommitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}

func resolveCommitFromLsRemote(output, requestedRef string) string {
	requestedRef = strings.TrimSpace(requestedRef)
	var fallback string
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || len(fields[0]) != 40 {
			continue
		}
		name := fields[1]
		sha := fields[0]
		if strings.HasSuffix(name, "^{}") {
			base := strings.TrimSuffix(name, "^{}")
			if requestedRef == "" || base == requestedRef || base == "refs/tags/"+requestedRef {
				return sha
			}
		}
		if requestedRef != "" && (name == requestedRef || name == "refs/heads/"+requestedRef || name == "refs/tags/"+requestedRef) {
			fallback = sha
		}
		if isFullCommitSHA(requestedRef) && sha == requestedRef {
			return sha
		}
	}
	return fallback
}

func (c *SafeGitCloner) ValidateGitURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid git url: %w", err)
	}

	if strings.ToLower(u.Scheme) != "https" {
		return fmt.Errorf("only HTTPS git urls are allowed, got: %s", u.Scheme)
	}

	host := strings.ToLower(u.Hostname())
	allowed := false
	for _, h := range c.allowHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("git host %s is not in allowlist %v", host, c.allowHosts)
	}

	// Resolve IPs to prevent DNS rebinding / SSRF to private addresses
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("failed to resolve git host %s: %w", host, err)
	}
	for _, ip := range ips {
		if isPrivateOrLocalIP(ip) {
			return fmt.Errorf("git host resolved to private or link-local address: %s (%s)", host, ip.String())
		}
	}

	return nil
}

func (c *SafeGitCloner) CloneTo(ctx context.Context, gitURL, ref, targetDir string) (string, error) {
	if err := c.ValidateGitURL(gitURL); err != nil {
		return "", err
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create target dir: %w", err)
	}

	cloneCtx, cancel := context.WithTimeout(ctx, c.cloneTimeout)
	defer cancel()

	// Shallow clone with depth=1
	cmdArgs := []string{"clone", "--depth", "1"}
	if ref != "" {
		cmdArgs = append(cmdArgs, "--branch", ref)
	}
	cmdArgs = append(cmdArgs, gitURL, targetDir)

	cmd := exec.CommandContext(cloneCtx, "git", cmdArgs...)
	// Prevent interactive prompts and hooks
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(targetDir)
		if errors.Is(cloneCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("git clone timed out after %v", c.cloneTimeout)
		}
		return "", fmt.Errorf("git clone failed: %v, output: %s", err, string(out))
	}

	// Get commit sha
	shaCmd := exec.CommandContext(ctx, "git", "-C", targetDir, "rev-parse", "HEAD")
	shaOut, err := shaCmd.Output()
	if err != nil {
		_ = os.RemoveAll(targetDir)
		return "", fmt.Errorf("failed to resolve exact cloned commit: %w", err)
	}
	commitSHA := strings.TrimSpace(string(shaOut))
	if len(commitSHA) != 40 {
		_ = os.RemoveAll(targetDir)
		return "", fmt.Errorf("git returned invalid commit SHA %q", commitSHA)
	}

	// Remove .git directory to keep source clean and prevent git-hook execution
	gitDir := filepath.Join(targetDir, ".git")
	_ = os.RemoveAll(gitDir)

	logger.L(ctx).Info("cloned repository snapshot successfully", "commit", commitSHA, "target", targetDir)
	return commitSHA, nil
}

// CloneCommitTo materializes an immutable commit without using a movable
// branch or tag. The caller can compare the returned HEAD with the resolved
// commit before publishing the directory.
func (c *SafeGitCloner) CloneCommitTo(ctx context.Context, gitURL, commitSHA, targetDir string) (string, error) {
	if !isFullCommitSHA(commitSHA) {
		return "", fmt.Errorf("invalid commit SHA %q", commitSHA)
	}
	if err := c.ValidateGitURL(gitURL); err != nil {
		return "", err
	}
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create target dir: %w", err)
	}
	cloneCtx, cancel := context.WithTimeout(ctx, c.cloneTimeout)
	defer cancel()
	commands := [][]string{
		{"init", targetDir},
		{"-C", targetDir, "remote", "add", "origin", gitURL},
		{"-C", targetDir, "fetch", "--depth", "1", "origin", commitSHA},
		{"-C", targetDir, "checkout", "--detach", "FETCH_HEAD"},
		{"-C", targetDir, "rev-parse", "HEAD"},
	}
	for index, args := range commands {
		cmd := exec.CommandContext(cloneCtx, "git", args...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		output, err := cmd.CombinedOutput()
		if err != nil {
			_ = os.RemoveAll(targetDir)
			if errors.Is(cloneCtx.Err(), context.DeadlineExceeded) {
				return "", fmt.Errorf("git exact commit materialization timed out after %v", c.cloneTimeout)
			}
			return "", fmt.Errorf("git exact commit command %d failed: %v, output: %s", index+1, err, string(output))
		}
		if index == len(commands)-1 {
			resolved := strings.TrimSpace(string(output))
			if resolved != commitSHA {
				_ = os.RemoveAll(targetDir)
				return "", fmt.Errorf("git returned %s for requested commit %s", resolved, commitSHA)
			}
		}
	}
	_ = os.RemoveAll(filepath.Join(targetDir, ".git"))
	return commitSHA, nil
}
