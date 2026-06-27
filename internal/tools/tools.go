// Package tools verifies external compression binaries are available on $PATH.
package tools

import (
	"fmt"
	"os/exec"
	"strings"
)

// Required lists binaries we depend on. One entry per media class.
var Required = []string{"jpegoptim", "oxipng", "cwebp", "gifsicle", "ffmpeg"}

// Check confirms every required binary is present and returns the missing ones.
func Check() error {
	var missing []string
	for _, b := range Required {
		if _, err := exec.LookPath(b); err != nil {
			missing = append(missing, b)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required binaries on $PATH: %s (see Dockerfile)", strings.Join(missing, ", "))
	}
	return nil
}
