package core

import (
	"strings"
)

// AgentTranscript — транскрипт агента: у суб-агента свой файл
// <транскрипт основного разговора без .jsonl>/subagents/agent-<id>.jsonl,
// у основной сессии — transcript. "" — пути нет или id не годится в имя файла
func AgentTranscript(transcript, agentID string) string {
	if transcript == "" {
		return ""
	}
	if agentID == "" {
		return transcript
	}
	id := strings.TrimSpace(agentID)
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\") {
		return ""
	}
	return strings.TrimSuffix(transcript, ".jsonl") + "/subagents/agent-" + id + ".jsonl"
}
