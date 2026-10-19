package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

// Every command's flags must merge with the global ones without a clash
// (cobra panics at run time otherwise).
func TestFlagSetsMerge(t *testing.T) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("%s: %v", c.CommandPath(), r)
			}
		}()
		_ = c.InheritedFlags()
		_ = c.LocalFlags()
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(NewRoot())
}
