package tools

import (
	"reflect"
	"testing"
)

func TestScriptCommandStringsConsumesEchoValue(t *testing.T) {
	got := scriptCommandStrings([]string{"-E", "-c", "not-the-command", "-c", "the-command"})
	want := []string{"the-command"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scriptCommandStrings() = %#v, want %#v", got, want)
	}
}
