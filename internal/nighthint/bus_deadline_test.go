package nighthint

import (
	"context"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/hookctx"
	"github.com/aiseeq/chooks/internal/testbus"
)

// ScreenLocked и LidClosed целиком на молчащей системной шине: каждый вызов
// кончается не позже lockTimeout + 100 мс с ошибкой, горутины и fd возвращаются
func TestLogindSilentBus(t *testing.T) {
	handlers := map[string]func(context.Context) (bool, error){
		"ScreenLocked": ScreenLocked,
		"LidClosed":    LidClosed,
	}
	for name, handler := range handlers {
		for _, addr := range testbus.Silent(t) {
			g0, f0 := testbus.Counts()
			for range 5 {
				req := hookctx.New(nil, nil, nil, []string{"DBUS_SYSTEM_BUS_ADDRESS=" + addr}, "/", 1, 2)
				start := time.Now()
				_, err := handler(hookctx.WithRequest(context.Background(), req))
				if el := time.Since(start); el > lockTimeout+100*time.Millisecond || err == nil {
					t.Fatalf("%s %s: err=%v elapsed=%v", name, addr, err, el)
				}
			}
			testbus.AssertReleased(t, name+" "+addr, g0, f0)
		}
	}
}
