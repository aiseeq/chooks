package questioncheck

import "testing"

// Сессия примера стенда — цель по имени; чужое имя — ошибка, а не пустая
// сессия: иначе судья стенда и хука видел бы разный вход
func TestTargetByName(t *testing.T) {
	targets := DefaultTargets("/home/u")
	got, err := TargetByName(targets, "work-admin")
	if err != nil || got.Dir != "/home/u/work" {
		t.Fatalf("work-admin = %+v, %v", got, err)
	}
	if got, err := TargetByName(targets, ""); err != nil || got.Name != "" {
		t.Fatalf("пусто = %+v, %v", got, err)
	}
	if _, err := TargetByName(targets, "glint-master"); err == nil {
		t.Fatal("чужая сессия должна быть ошибкой")
	}
}
