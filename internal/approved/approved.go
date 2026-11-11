// Package approved — список одобренных моделей ~/.config/models/approved:
// строки «<имя> <id>», комментарий с «#» до конца строки. Модели вне списка
// не используются нигде (пользователь 21.10)
package approved

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Model — строка списка: имя для worker --model и id, который уходит клиенту
type Model struct {
	Name string
	ID   string
	// Line — номер строки в файле: для сообщения о плохом id
	Line int
}

// Path — путь списка: MODELS_APPROVED (подмена в тестах и пробах, как у
// worker из llmark), иначе ~/.config/models/approved
func Path() (string, error) {
	if p := os.Getenv("MODELS_APPROVED"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve home dir for the approved models list: %w", err)
	}
	return filepath.Join(home, ".config", "models", "approved"), nil
}

// ErrEmpty — в списке нет ни одной модели
var ErrEmpty = errors.New("approved models list is empty")

// Read читает список. Строка не из двух полей — ошибка с номером строки
func Read(path string) ([]Model, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open the approved models list: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var models []Model
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		if text == "" {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s, строка %d: ожидаются ровно два поля «<имя> <id>»", path, line)
		}
		models = append(models, Model{Name: fields[0], ID: fields[1], Line: line})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("%s: %w", path, ErrEmpty)
	}
	return models, nil
}
