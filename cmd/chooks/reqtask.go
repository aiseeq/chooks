package main

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
)

// daemonCallKey — ключ ctx запроса, исполняемого демоном chooks serve
type daemonCallKey struct{}

// daemonCall — то, что демон даёт запросу сверх hookctx.Context:
// terminated закрывается, когда прокси сообщил о своём SIGTERM ('T');
// orphan получает панику горутины запроса, которую запрос уже не ждёт
type daemonCall struct {
	terminated <-chan struct{}
	orphan     func(value any, stack []byte)
}

// goroutinePanic — паника горутины запроса, поднятая снова в горутине,
// которая её ждала: стек — исходный
type goroutinePanic struct {
	value any
	stack []byte
}

func (p goroutinePanic) Error() string { return fmt.Sprint(p.value) }

// requestTask — горутина, которую обработчик хука запускает и ждёт до своего
// срока (проверка skill-reload, чтение записи для night-hint). Паника в ней
// не роняет процесс демона: пока её ждут, она поднимается снова в ждущей
// горутине — запрос отвечает как CLI на панике (код 2, стек в stderr); после
// ухода ждущего — уходит orphan запроса (у демона — строка Error в его лог),
// в CLI — роняет процесс, как раньше. Результат горутина отдаёт через
// замыкание, которое ждущий читает только после done
type requestTask struct {
	done     chan struct{}
	orphan   func(value any, stack []byte)
	mu       sync.Mutex
	left     bool
	panicked *goroutinePanic
}

// goRequest запускает fn в своей горутине
func goRequest(ctx context.Context, fn func()) *requestTask {
	t := &requestTask{done: make(chan struct{}), orphan: rethrowOrphan}
	if call, ok := ctx.Value(daemonCallKey{}).(*daemonCall); ok && call.orphan != nil {
		t.orphan = call.orphan
	}
	go func() {
		defer close(t.done)
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			p := &goroutinePanic{value: r, stack: debug.Stack()}
			if inner, ok := r.(goroutinePanic); ok {
				p = &inner
			}
			t.mu.Lock()
			t.panicked = p
			left := t.left
			t.mu.Unlock()
			if left {
				t.orphan(p.value, p.stack)
			}
		}()
		fn()
	}()
	return t
}

// rethrowOrphan — паника брошенной горутины в CLI: процесс падает, как без
// перехвата
func rethrowOrphan(value any, stack []byte) {
	panic(goroutinePanic{value: value, stack: stack})
}

// wait — горутина кончилась (после <-t.done): её паника поднимается здесь
func (t *requestTask) wait() {
	<-t.done
	if t.panicked != nil {
		panic(*t.panicked)
	}
}

// leave — ждущий уходит, не дождавшись. Паника, уже случившаяся, поднимается
// здесь: запрос ещё не ответил
func (t *requestTask) leave() {
	t.mu.Lock()
	t.left = true
	p := t.panicked
	t.mu.Unlock()
	if p != nil {
		panic(*p)
	}
}
