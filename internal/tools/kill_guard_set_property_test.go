package tools

import "testing"

// Тикет chooks-set-property-not-signal: предел IO и CPU юнита бойца его
// замедляет, а не гасит — пропуск; остановка, сигнал и предел памяти или
// задач — отказ, как было
func TestKillGuardWorkerSetPropertyThrottle(t *testing.T) {
	for _, command := range []string{
		`systemctl --user set-property --runtime worker-round-x-1.service "IOReadBandwidthMax=/dev/sda 60M"`,
		`systemctl --user set-property worker-round-x-1.service "IOWriteBandwidthMax=/dev/nvme0n1 20M" IOWeight=10`,
		`systemctl --user set-property --runtime worker-round-x-1.service CPUQuota=50% CPUWeight=20`,
		`systemctl --user set-property worker-round-x-1.service AllowedCPUs=0-3`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
	for _, command := range []string{
		`systemctl --user stop worker-round-x-1.service`,
		`systemctl --user kill worker-round-x-1.service`,
		`systemctl --user set-property worker-round-x-1.service MemoryMax=1M`,
		`systemctl --user set-property worker-round-x-1.service MemoryHigh=1M`,
		`systemctl --user set-property worker-round-x-1.service TasksMax=1`,
		`systemctl --user set-property worker-round-x-1.service CPUQuota=50% MemoryMax=1M`,
		`systemctl --user set-property worker-round-x-1.service "$P"`,
		// без кавычек X="1 MemoryMax=1M" даёт второе свойство
		`systemctl --user set-property worker-round-x-1.service IOWeight=$X`,
		`systemctl --user set-property worker-round-x-1.service`,
		`systemctl --user set-property agent-x-1.scope "IOReadBandwidthMax=/dev/sda 60M"`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
}

// Склейка коротких опций: -ps — -p со значением «s», следующее слово —
// операнд (проверено на systemctl 18.11: set-property … -ps TasksMax=77
// ставит TasksMax, -ps stop останавливает юнит)
func TestKillGuardSystemctlOptionCluster(t *testing.T) {
	for _, command := range []string{
		`systemctl --user set-property worker-x.service IOWeight=10 -ps MemoryMax=1M`,
		`systemctl --user set-property worker-x.service IOWeight=10 -Ps TasksMax=1`,
		`systemctl --user set-property worker-x.service IOWeight=10 -pt MemoryMax=1M`,
		`systemctl --user -ps stop agent-x-1.scope`,
		`systemctl --user stop -ps agent-x-1.scope`,
	} {
		if validateKill(t, command).IsValid {
			t.Errorf("должно блокироваться: %s", command)
		}
	}
	for _, command := range []string{
		`systemctl --user set-property worker-x.service IOWeight=10 -p MemoryMax`,
		`systemctl --user -qp MainPID show agent-x-1.scope`,
	} {
		if r := validateKill(t, command); !r.IsValid {
			t.Errorf("не должно блокироваться: %s: %v", command, r.Suggestions)
		}
	}
}
