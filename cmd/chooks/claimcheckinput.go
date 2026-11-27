package main

import (
	"encoding/json"
	"fmt"

	"github.com/aiseeq/chooks/internal/claimcheck"
	"github.com/spf13/cobra"
)

func newClaimCheckInputCmd() *cobra.Command {
	var transcript string
	var line, contextLimit int
	var unbounded, callsOnly bool
	cmd := &cobra.Command{
		Use:   "claim-check-input --transcript <jsonl> --line <N>",
		Short: "Построить вход судьи из транскрипта на указанном событии",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if transcript == "" || line < 1 {
				return fmt.Errorf("--transcript and positive --line are required")
			}
			input, err := claimcheck.BuildInput(cmd.Context(), transcript, line, claimcheck.Options{ContextLimit: contextLimit, Unbounded: unbounded, CallsOnly: callsOnly})
			if err != nil {
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetEscapeHTML(false)
			return enc.Encode(input)
		},
	}
	cmd.Flags().StringVar(&transcript, "transcript", "", "path to Claude Code JSONL transcript")
	cmd.Flags().IntVar(&line, "line", 0, "one-based physical JSONL event line")
	cmd.Flags().IntVar(&contextLimit, "context-limit", claimcheck.DefaultContextLimit, "maximum context size in characters")
	cmd.Flags().BoolVar(&unbounded, "unbounded", false, "build full input for visibility measurement")
	cmd.Flags().BoolVar(&callsOnly, "calls-only", false, "include calls without outputs")
	if err := cmd.Flags().MarkHidden("unbounded"); err != nil {
		panic(err)
	}
	return cmd
}
