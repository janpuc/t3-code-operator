package t3client

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const versionCommandTimeout = 10 * time.Second

var t3VersionPattern = regexp.MustCompile(`^t3 v([0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?)$`)

func DetectVersion(ctx context.Context, binary string) (string, error) {
	if binary == "" {
		binary = "t3"
	}
	commandContext, cancel := context.WithTimeout(ctx, versionCommandTimeout)
	defer cancel()
	command := exec.CommandContext(commandContext, binary, "--version")
	command.Env = authCommandEnvironment()
	command.Stdin = bytes.NewReader(nil)
	standardOutput := &authCommandBuffer{limit: authCommandOutputLimit}
	standardError := &authCommandBuffer{limit: authCommandOutputLimit}
	command.Stdout = standardOutput
	command.Stderr = standardError
	if err := command.Run(); err != nil {
		return "", errors.New("t3 version command failed")
	}
	if standardOutput.truncated || standardError.truncated {
		return "", errors.New("t3 version command output exceeded its size limit")
	}
	output := strings.TrimSpace(standardOutput.String())
	if output == "" {
		output = strings.TrimSpace(standardError.String())
	}
	match := t3VersionPattern.FindStringSubmatch(output)
	if len(match) != 2 {
		return "", errors.New("t3 version command returned an invalid version")
	}
	return match[1], nil
}
