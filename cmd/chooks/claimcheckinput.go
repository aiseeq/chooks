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
	cmd := &cobra.Command{
		Use:   "claim-check-input --transcript <jsonl> --line <N>",
		Short: "Построить вход судьи из транскрипта на указанном событии",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if transcript == "" || line < 1 {
				return fmt.Errorf("--transcript and positive --line are required")
			}
			input, err := claimcheck.BuildInput(transcript, line, claimcheck.Options{ContextLimit: contextLimit})
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
	return cmd
}
