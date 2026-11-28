//go:build race

package claimcheck

// raceBuild — тесты собраны с -race: мерные тесты сборки входа числа только
// пишут в журнал (budget_measure_test.go, allocMargin)
const raceBuild = true
