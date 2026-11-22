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

// ConnectBus bounds both dialing and the D-Bus authentication handshake.
// Nonce transports are treated as unavailable rather than starting an
// uninterruptible nonce-file read in the serving process.
func (c Context) ConnectBus(ctx context.Context, address string) (*dbus.Conn, error) {
	var last error
	for entry := range strings.SplitSeq(c.BusAddress(address), ";") {
		transport, keys, ok := strings.Cut(entry, ":")
		if !ok || (transport != "unix" && transport != "tcp") {
			last = fmt.Errorf("unsupported D-Bus address")
			continue
		}
		values := map[string]string{}
		invalid := false
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
				last = err
				invalid = true
				break
			}
			values[key] = value
		}
		if invalid {
			continue
		}
		network, target := "unix", values["path"]
		if transport == "unix" {
			abstract := values["abstract"]
			if (target == "") == (abstract == "") {
				last = fmt.Errorf("invalid D-Bus unix address")
				continue
			}
			if abstract != "" {
				target = "@" + abstract
			}
		} else {
			switch values["family"] {
			case "":
				network = "tcp"
			case "ipv4":
				network = "tcp4"
			case "ipv6":
				network = "tcp6"
			default:
				last = fmt.Errorf("invalid D-Bus tcp family")
				continue
			}
			if values["host"] == "" || values["port"] == "" {
				last = fmt.Errorf("invalid D-Bus tcp address")
				continue
			}
			target = net.JoinHostPort(values["host"], values["port"])
		}
		raw, err := (&net.Dialer{}).DialContext(ctx, network, target)
		if err != nil {
			last = err
			continue
		}
		stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
		var conn *dbus.Conn
		if transport == "unix" {
			unixConn, ok := raw.(*net.UnixConn)
			if !ok {
				stop()
				_ = raw.Close()
				last = fmt.Errorf("unexpected D-Bus unix connection type")
				continue
			}
			conn, err = dbus.ConnectUnix(unixConn, dbus.WithContext(ctx))
		} else {
			conn, err = dbus.NewConn(raw, dbus.WithContext(ctx))
			if err == nil {
				err = conn.Auth(nil)
			}
			if err == nil {
				err = conn.Hello()
			}
			if err != nil && conn != nil {
				_ = conn.Close()
			}
		}
		if !stop() && err == nil {
			_ = conn.Close()
			return nil, ctx.Err()
		}
		if err == nil {
			return conn, nil
		}
		_ = raw.Close()
		last = err
	}
	if last == nil {
		last = fmt.Errorf("empty D-Bus address")
	}
	return nil, last
}
