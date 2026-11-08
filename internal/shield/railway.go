package shield

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// RailwayEnabler returns an Enabler that runs the Railway CLI, authenticated
// by the RAILWAY_TOKEN project token already in the environment. serviceID is
// the service to protect; bin is the CLI binary.
func RailwayEnabler(bin, serviceID string) Enabler {
	return func(ctx context.Context) error {
		args := []string{"waf", "under-attack", "enable", "--duration", "1h"}
		if serviceID != "" {
			args = append(args, "--service", serviceID)
		}
		out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("railway %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}
