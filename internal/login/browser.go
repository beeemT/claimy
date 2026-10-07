package login

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"time"
)

const browserLaunchTimeout = 10 * time.Second

func launchBrowser(ctx context.Context, authorizationURL string, stderr io.Writer) error {
	if stderr != nil {
		if _, err := fmt.Fprintln(stderr, "Open this URL in a browser:", authorizationURL); err != nil {
			return errors.New("write login authorization URL failed")
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	launchCtx, cancel := context.WithTimeout(ctx, browserLaunchTimeout)
	defer cancel()

	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.CommandContext(launchCtx, "open", authorizationURL)
	case "windows":
		command = exec.CommandContext(launchCtx, "rundll32", "url.dll,FileProtocolHandler", authorizationURL)
	default:
		command = exec.CommandContext(launchCtx, "xdg-open", authorizationURL)
	}
	if err := command.Run(); err != nil {
		const manualFallback = "Automatic browser launch failed. Open the authorization URL manually."
		if stderr == nil {
			return errors.New("automatic browser launch failed; open the authorization URL manually")
		}
		if _, writeErr := fmt.Fprintln(stderr, manualFallback); writeErr != nil {
			return errors.New("write login browser fallback message failed")
		}
	}

	return nil
}
