package desktop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	notifyService = "org.freedesktop.Notifications"
	notifyPath    = "/org/freedesktop/Notifications"
	notifyIface   = "org.freedesktop.Notifications"

	// defaultActionKey — действие, которое KDE вызывает при клике по телу уведомления,
	// а не только по кнопке
	defaultActionKey = "default"
)

// Urgency — срочность уведомления по спецификации freedesktop (hint urgency)
type Urgency byte

const (
	UrgencyLow    Urgency = 0
	UrgencyNormal Urgency = 1
	// UrgencyCritical — KDE Plasma показывает такое уведомление с оранжевой
	// полосой и не закрывает само, пока его не закроет пользователь
	UrgencyCritical Urgency = 2
)

// String возвращает имя срочности, которое принимает ParseUrgency
func (u Urgency) String() string {
	switch u {
	case UrgencyLow:
		return "low"
	case UrgencyNormal:
		return "normal"
	case UrgencyCritical:
		return "critical"
	}
	return fmt.Sprintf("urgency(%d)", byte(u))
}

// ParseUrgency разбирает имя срочности: low, normal или critical
func ParseUrgency(name string) (Urgency, error) {
	for _, urgency := range []Urgency{UrgencyLow, UrgencyNormal, UrgencyCritical} {
		if urgency.String() == name {
			return urgency, nil
		}
	}
	return 0, fmt.Errorf("unknown urgency %q: want low, normal or critical", name)
}

// Notification описывает отправляемое уведомление
type Notification struct {
	AppName string
	Icon    string
	Title   string
	Message string
	// Timeout 0 — уведомление не истекает само
	Timeout time.Duration
	Urgency Urgency
	// ActionLabel непустой, если по клику нужно выполнить действие
	ActionLabel string
}

// Send отправляет уведомление и возвращает его идентификатор
func Send(conn *dbus.Conn, notification Notification) (uint32, error) {
	var actions []string
	if notification.ActionLabel != "" {
		actions = []string{defaultActionKey, notification.ActionLabel}
	}

	hints := map[string]dbus.Variant{
		"urgency": dbus.MakeVariant(byte(notification.Urgency)),
	}

	var id uint32
	err := conn.Object(notifyService, notifyPath).Call(
		notifyIface+".Notify", 0,
		notification.AppName,
		uint32(0), // не заменяем предыдущее уведомление
		notification.Icon,
		notification.Title,
		notification.Message,
		actions,
		hints,
		int32(notification.Timeout.Milliseconds()),
	).Store(&id)
	if err != nil {
		return 0, fmt.Errorf("failed to send notification: %w", err)
	}

	return id, nil
}

// WaitForAction ждёт клика по уведомлению с идентификатором id.
// Возвращает true, если пользователь нажал на уведомление, и false,
// если оно было закрыто, истекло или истёк срок ожидания
func WaitForAction(ctx context.Context, conn *dbus.Conn, id uint32) (clicked bool, err error) {
	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath(notifyPath),
		dbus.WithMatchInterface(notifyIface),
	); err != nil {
		return false, fmt.Errorf("failed to subscribe to notification signals: %w", err)
	}
	defer func() {
		if removeErr := conn.RemoveMatchSignal(
			dbus.WithMatchObjectPath(notifyPath),
			dbus.WithMatchInterface(notifyIface),
		); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("failed to unsubscribe from notification signals: %w", removeErr))
		}
	}()

	signals := make(chan *dbus.Signal, 16)
	conn.Signal(signals)
	defer conn.RemoveSignal(signals)

	for {
		select {
		case <-ctx.Done():
			return false, nil

		case signal, ok := <-signals:
			if !ok {
				return false, nil
			}

			switch signal.Name {
			case notifyIface + ".ActionInvoked":
				if signalID(signal) == id {
					return true, nil
				}

			case notifyIface + ".NotificationClosed":
				// Закрытое уведомление больше не может быть нажато
				if signalID(signal) == id {
					return false, nil
				}
			}
		}
	}
}

// signalID извлекает идентификатор уведомления из сигнала
func signalID(signal *dbus.Signal) uint32 {
	if len(signal.Body) == 0 {
		return 0
	}
	id, _ := signal.Body[0].(uint32)
	return id
}
