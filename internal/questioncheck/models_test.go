package questioncheck

import (
	"os"
	"path/filepath"
	"testing"
)

// Судья берёт список из MODELS_APPROVED, как model_guard и worker: проба с
// подменным списком видит ту же модель, что хук (ревизия хуков 11.11)
func TestNewCLIJudgeReadsModelsApproved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approved")
	if err := os.WriteFile(path, []byte("probe probe-id-1  # проба\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MODELS_APPROVED", path)
	judge, err := NewCLIJudge("probe", "low", t.TempDir())
	if err != nil || judge.ID != "probe-id-1" {
		t.Fatalf("judge = %+v, err = %v", judge, err)
	}
	if _, err := NewCLIJudge("opus", "low", t.TempDir()); err == nil {
		t.Fatal("модель вне списка MODELS_APPROVED принята")
	}
	if _, err := NewCLIJudge("probe", "extreme", t.TempDir()); err == nil {
		t.Fatal("неизвестное усилие принято")
	}
}
