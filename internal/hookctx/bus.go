package hookctx

import (
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
		if value == "" {
			return keys
		}
		if resolved := c.Abs(value); resolved != value {
			pairs[i] = key + "=" + dbus.EscapeBusAddressValue(resolved)
		}
		return strings.Join(pairs, ",")
	}
	return keys
}
