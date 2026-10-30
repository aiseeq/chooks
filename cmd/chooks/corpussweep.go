package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/aiseeq/chooks/internal/tools"
)

func newCorpusSweepCmd() *cobra.Command {
	var input, field string
	cmd := &cobra.Command{
		Use:   "corpus-sweep-check --input FILE [--field command]",
		Short: "Проверить JSONL-команды на поиск по корпусу транскриптов",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if input == "" {
				return fmt.Errorf("нужно указать --input FILE")
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			file, err := os.Open(input)
			if err != nil {
				return err
			}
			defer file.Close()
			return runCorpusSweep(cmd.OutOrStdout(), cmd.ErrOrStderr(), file, field, home)
		},
	}
	cmd.Flags().StringVar(&input, "input", "", "входной файл JSONL")
	cmd.Flags().StringVar(&field, "field", "command", "поле объекта с командой")
	return cmd
}

func runCorpusSweep(out, summary io.Writer, input io.Reader, field, home string) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	encoder := json.NewEncoder(out)
	var total, blocked, points, withoutCorpus int
	for line := 1; scanner.Scan(); line++ {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &object); err != nil || object == nil {
			if err == nil {
				err = fmt.Errorf("ожидался объект JSON")
			}
			return fmt.Errorf("строка %d: некорректный JSON: %w", line, err)
		}
		raw, ok := object[field]
		var command string
		if !ok || json.Unmarshal(raw, &command) != nil {
			return fmt.Errorf("строка %d: поле %q отсутствует или не является строкой", line, field)
		}
		verdict := tools.CheckCorpusSweep(command, home)
		if verdict.Sweeps == nil {
			verdict.Sweeps = []string{}
		}
		if verdict.Points == nil {
			verdict.Points = []string{}
		}
		annotation, err := json.Marshal(verdict)
		if err != nil {
			return fmt.Errorf("строка %d: сериализация вердикта: %w", line, err)
		}
		object["corpus_sweep"] = annotation
		if err := encoder.Encode(object); err != nil {
			return fmt.Errorf("строка %d: запись результата: %w", line, err)
		}
		total++
		if verdict.Blocked {
			blocked++
		} else if len(verdict.Points) > 0 {
			points++
		} else {
			withoutCorpus++
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("чтение входного файла: %w", err)
	}
	_, err := fmt.Fprintf(summary, "Всего: %d; остановлено: %d; точечных среди пропущенных: %d; без корпуса: %d\n", total, blocked, points, withoutCorpus)
	return err
}
