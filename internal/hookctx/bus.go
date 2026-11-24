package hookctx

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/godbus/dbus/v5"
)

// busPathKeys — ключи адреса D-Bus, значения которых godbus открывает как
// путь файловой системы: unix:path= сокета и nonce-tcp:noncefile=
var busPathKeys = map[string]string{
	"unix":      "path",
	"nonce-tcp": "noncefile",
}

// BusAddress разрешает относительные пути в адресе D-Bus как Abs: godbus
// открывает их через ОС от cwd процесса, а у запроса демона cwd — свой.
// Разбор повторяет godbus (conn.go getTransport, getKey): записи через «;»,
// транспорт до первого «:», ключи через «,», берётся первое значение ключа.
// У CLI Abs не меняет путь, и адрес остаётся как дан, как в базе.
func (c Context) BusAddress(address string) string {
	entries := strings.Split(address, ";")
	for i, entry := range entries {
		transport, keys, ok := strings.Cut(entry, ":")
		if !ok {
			continue
		}
		key, ok := busPathKeys[transport]
		if !ok {
			continue
		}
		entries[i] = transport + ":" + c.busKeys(keys, key)
	}
	return strings.Join(entries, ";")
}

func (c Context) busKeys(keys, key string) string {
	pairs := strings.Split(keys, ",")
	for i, pair := range pairs {
		raw, ok := strings.CutPrefix(pair, key+"=")
		if !ok {
			continue
		}
		value, err := dbus.UnescapeBusAddressValue(raw)
		//nolint:silent-error-handling // ключ остаётся как дан: godbus сам не разберёт это значение и вернёт ошибку адреса
		if err != nil {
			return keys
		}
		// Пустое значение godbus считает незаданным ключом: от cwd его не разрешать
		if value == "" || (key == "path" && strings.HasPrefix(value, "@")) {
			return keys
		}
		if resolved := c.Abs(value); resolved != value {
			pairs[i] = key + "=" + dbus.EscapeBusAddressValue(resolved)
		}
		return strings.Join(pairs, ",")
	}
	return keys
}

// ConnectBus bounds dialing, the D-Bus authentication handshake and Hello by
// ctx: on expiry the raw socket is closed (AfterFunc). The returned connection
// is not tied to ctx (no dbus.WithContext): callers such as the Konsole title
// keep using it after the connecting function returned.
// Nonce transports are treated as unavailable rather than starting an
// uninterruptible nonce-file read in the serving process.
func (c Context) ConnectBus(ctx context.Context, address string) (*dbus.Conn, error) {
	var last error
	for entry := range strings.SplitSeq(c.BusAddress(address), ";") {
		target, valid, err := parseBusTarget(entry)
		if !valid {
			last = err
			continue
		}
		conn, retry, err := c.connectBusTarget(ctx, target)
		if retry {
			last = err
			continue
		}
		return conn, err
	}
	if last == nil {
		last = fmt.Errorf("empty D-Bus address")
	}
	return nil, last
}

type busTarget struct {
	transport string
	network   string
	address   string
}

func parseBusTarget(entry string) (busTarget, bool, error) {
	transport, keys, ok := strings.Cut(entry, ":")
	if !ok || (transport != "unix" && transport != "tcp") {
		return busTarget{}, false, fmt.Errorf("unsupported D-Bus address")
	}
	values := map[string]string{}
	for pair := range strings.SplitSeq(keys, ",") {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		if _, exists := values[key]; exists {
			continue
		}
		value, err := dbus.UnescapeBusAddressValue(value)
		if err != nil {
			return busTarget{}, false, fmt.Errorf("decode D-Bus address value: %w", err)
		}
		values[key] = value
	}
	if transport == "unix" {
		return unixBusTarget(values)
	}
	return tcpBusTarget(values)
}

func unixBusTarget(values map[string]string) (busTarget, bool, error) {
	path, abstract := values["path"], values["abstract"]
	if (path == "") == (abstract == "") {
		return busTarget{}, false, fmt.Errorf("invalid D-Bus unix address")
	}
	if abstract != "" {
		path = "@" + abstract
	}
	return busTarget{transport: "unix", network: "unix", address: path}, true, nil
}

func tcpBusTarget(values map[string]string) (busTarget, bool, error) {
	network := "tcp"
	switch values["family"] {
	case "":
	case "ipv4":
		network = "tcp4"
	case "ipv6":
		network = "tcp6"
	default:
		return busTarget{}, false, fmt.Errorf("invalid D-Bus tcp family")
	}
	if values["host"] == "" || values["port"] == "" {
		return busTarget{}, false, fmt.Errorf("invalid D-Bus tcp address")
	}
	return busTarget{transport: "tcp", network: network, address: net.JoinHostPort(values["host"], values["port"])}, true, nil
}

func (c Context) connectBusTarget(ctx context.Context, target busTarget) (*dbus.Conn, bool, error) {
	raw, err := (&net.Dialer{}).DialContext(ctx, target.network, target.address)
	if err != nil {
		return nil, true, fmt.Errorf("dial D-Bus %s: %w", target.network, err)
	}
	if target.transport == "unix" {
		if _, ok := raw.(*net.UnixConn); !ok {
			_ = raw.Close()
			return nil, true, fmt.Errorf("unexpected D-Bus unix connection type")
		}
	}
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	conn, err := busConnection(target.transport, raw)
	if !stop() && err == nil {
		_ = conn.Close()
		return nil, false, ctx.Err()
	}
	if err == nil {
		return conn, false, nil
	}
	// Сокет открылся, а auth или Hello не прошли: godbus не идёт к следующему адресу
	_ = raw.Close()
	return nil, false, err
}

func busConnection(transport string, raw net.Conn) (*dbus.Conn, error) {
	if transport == "unix" {
		unixConn, ok := raw.(*net.UnixConn)
		if !ok {
			return nil, fmt.Errorf("unexpected D-Bus unix connection type")
		}
		return dbus.ConnectUnix(unixConn)
	}
	conn, err := dbus.NewConn(raw)
	if err == nil {
		err = conn.Auth(nil)
	}
	if err == nil {
		err = conn.Hello()
	}
	if err != nil && conn != nil {
		_ = conn.Close()
	}
	return conn, err
}
