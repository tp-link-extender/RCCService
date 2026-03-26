package shared

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	c "github.com/TwiN/go-color"
)

func Log(txt string) {
	// Hey, Go date formatting isn't so bad
	fmt.Println(time.Now().Format("2006/01/02, 15:04:05 "), txt)
}

func Fatal(err error, txt string) {
	// so that I don't have to write this every time
	if err == nil {
		return
	}
	fmt.Println(err)
	Log(c.InRed(txt))
	os.Exit(1)
}

const display = ":0"

// this is needed on linux because Studio is a GUI application
func StartDisplayServer() error {
	if err := os.Setenv("DISPLAY", display); err != nil {
		return fmt.Errorf("set DISPLAY environment variable: %w", err)
	}

	cmd := exec.Command("weston", "-B", "headless")
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Weston: %w", err)
	}

	Log(c.InGreen(fmt.Sprintf("Display server started on %s", display)))
	return nil
}
