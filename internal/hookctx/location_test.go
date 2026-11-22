package hookctx

import (
	"sync"
	"testing"
	"time"
)

func TestRequestTimezonesParallel(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		env  string
		hour int
	}{{"TZ=UTC", 12}, {"TZ=Asia/Tokyo", 21}, {"TZ=America/New_York", 7}, {"TZ=", 12}, {"TZ=not/a-zone", 12}}
	var wg sync.WaitGroup
	for i := range 50 {
		tc := cases[i%len(cases)]
		wg.Go(func() {
			req := New(nil, nil, nil, []string{tc.env}, "/", 1, 2)
			if hour := now.In(req.Location()).Hour(); hour != tc.hour {
				t.Errorf("%s: hour %d, want %d", tc.env, hour, tc.hour)
			}
		})
	}
	wg.Wait()
}
