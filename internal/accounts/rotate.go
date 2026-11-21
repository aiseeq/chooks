package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aiseeq/chooks/internal/core"
	"github.com/aiseeq/chooks/internal/desktop"
	"github.com/aiseeq/chooks/internal/hookctx"
)

// Сроки ротации
const (
	// settle — файл токенов сверяется, когда он не менялся столько: /login
	// пишет токены и учётку не одним движением
	settle = 5 * time.Second
	// switchCooldown — пауза после переключения: сессии успевают сходить
	// в API с новой учёткой, и её доля доходит до снимка
	switchCooldown = 2 * time.Minute
	// retryDelay — пауза после сетевой ошибки сверки
	retryDelay = 5 * time.Minute
	// spawnInterval — фоновая ротация запускается не чаще
	spawnInterval = 15 * time.Second
	// switchMargin — токен доступа учётки, на которую переключаемся, должен
	// жить ещё столько, иначе он обновляется до переключения
	switchMargin = 10 * time.Minute
	// usageMargin — то же для запроса расхода неактивной учётки
	usageMargin = 2 * time.Minute
)

// Limits — снимок долей строки статуса: откуда ротация берёт расход учёток
// и куда пишет расход, спрошенный у API
type Limits interface {
	Usage() (map[string]Usage, error)
	Record(email string, week Window, now time.Time) error
}

// Rotator ведёт хранилище и переключает учётки
type Rotator struct {
	Paths  Paths
	Client *Client
	Limits Limits
	Log    core.Logger
	Now    func() time.Time
}

// Options — режим прогона
type Options struct {
	// Force — сверить файл токенов сразу, без ожидания settle и паузы после
	// сетевой ошибки (команда save)
	Force bool
	// Wait — ждать, если ротацию ведёт другой процесс
	Wait bool
}

// Rotate сверяет хранилище с файлом Claude Code, спрашивает расход учёток
// с неизвестной неделей и переключает на учётку по правилу выбора.
// false — ротацию уже ведёт другой процесс
func (r *Rotator) Rotate(ctx context.Context, opts Options) (bool, error) {
	if err := os.MkdirAll(r.Paths.StateDir, 0o755); err != nil {
		return false, fmt.Errorf("cannot create the state directory: %w", err)
	}
	unlock, ok, err := flock(r.Paths.storeLock(), opts.Wait)
	if err != nil || !ok {
		return ok, err
	}
	defer unlock()

	s, err := loadStore(r.Paths)
	if err != nil {
		return true, err
	}
	syncErr := r.sync(ctx, s, opts.Force)
	if syncErr != nil {
		r.Log.Warn("accounts: credentials not synced", "error", syncErr)
	}
	// Замер недель — и при выключенном автопереключении: после ручного
	// switch и /login на учётку со сброшенной неделей запись иначе остаётся
	// на старой доле (13.11)
	if syncErr == nil {
		if len(s.Accounts) < 2 {
			r.Log.Info("accounts: no switch", "reason", "нужны минимум две сохранённые учётки")
		} else if usage, ok := r.measure(ctx, s); ok {
			if s.AutoOff {
				r.Log.Info("accounts: no switch", "reason", "автопереключение выключено")
			} else {
				r.autoSwitch(ctx, s, usage)
			}
		}
	}
	if err := saveStore(r.Paths, s); err != nil {
		return true, err
	}
	return true, syncErr
}

// measure спрашивает у API недели учёток, которых нет в снимке или они
// пусты (needsUsage), и возвращает снимок после замера; false — снимок не
// прочесть
func (r *Rotator) measure(ctx context.Context, s *Store) (map[string]Usage, bool) {
	now := r.Now()
	usage, err := r.Limits.Usage()
	if err != nil {
		r.Log.Warn("accounts: limits snapshot unreadable, no measure and no switch", "error", err)
		return nil, false
	}
	if now.After(s.RetryAt) {
		for _, a := range needsUsage(s, usage, now) {
			if err := r.fetchUsage(ctx, s, a); err != nil {
				r.Log.Warn("accounts: usage not fetched", "account", a.Email, "error", err)
			}
		}
		if usage, err = r.Limits.Usage(); err != nil {
			r.Log.Warn("accounts: limits snapshot unreadable, no measure and no switch", "error", err)
			return nil, false
		}
	}
	return usage, true
}

// autoSwitch переключает по правилу выбора по снимку расхода usage
func (r *Rotator) autoSwitch(ctx context.Context, s *Store, usage map[string]Usage) {
	now := r.Now()
	activeAlive := false
	if active := s.byUUID(s.Active); active != nil {
		activeAlive = eligible(active, now)
	}
	if activeAlive && now.Sub(s.LastSwitch) < switchCooldown {
		r.Log.Info("accounts: no switch", "reason", fmt.Sprintf("кулдаун после переключения до %s", s.LastSwitch.Add(switchCooldown).Format(time.RFC3339)))
		return
	}
	// Цель может оказаться мёртвой при обновлении — тогда выбор повторяется
	for range s.Accounts {
		choice := chooseResult(s, usage, now)
		target, reason := choice.account, choice.reason
		if target == nil {
			r.Log.Warn("accounts: no switch", "reasons", strings.Join(noSwitchReasons(s, usage, now), "; "))
			return
		}
		if target.UUID == s.Active {
			if choice.outcome == chooseAllEmpty {
				r.Log.Warn("accounts: no switch", "reasons", strings.Join(noSwitchReasons(s, usage, now), "; "))
			} else {
				r.Log.Info("accounts: no switch", "reason", reason)
			}
			return
		}
		err := r.switchTo(ctx, s, target, reason, "rotation")
		if err == nil {
			return
		}
		if errors.Is(err, errJournalAfterSwitch) {
			return
		}
		r.Log.Warn("accounts: switch failed", "to", target.Email, "error", err)
		if target.Dead == "" {
			return
		}
	}
}

// sync сверяет хранилище с файлом токенов: обновлённый Claude Code токен
// активной учётки уходит в хранилище, новый вход (/login) опознаётся по
// API и сохраняется, стёртый Claude Code вход помечается мёртвым
func (r *Rotator) sync(ctx context.Context, s *Store, force bool) error {
	now := r.Now()
	creds, err := readCredentials(r.Paths.Credentials())
	if err != nil {
		return err
	}
	if creds.mtime.Equal(s.SyncedMtime) && !force {
		return nil
	}
	if !force && now.Sub(creds.mtime) < settle {
		return nil
	}
	raw := creds.oauthRaw()
	if len(raw) == 0 {
		// Выход (/logout): активной учётки нет, хранилище не трогается
		r.Log.Info("accounts: no login in Claude Code credentials")
		if err := r.recordExternal(s, "", "", now, creds.mtime); err != nil {
			return err
		}
		s.Active, s.SyncedMtime = "", creds.mtime
		return nil
	}
	o, err := parseOAuth(raw)
	if err != nil {
		return err
	}
	if o.cleared() {
		if a := s.byUUID(s.Active); a != nil && a.Dead == "" {
			a.Dead = "Claude Code стёр токены: обновление отклонено, нужен /login"
			r.Log.Warn("accounts: active login is dead", "account", a.Email)
		}
		s.SyncedMtime = creds.mtime
		return nil
	}
	known, err := s.byLogin(o)
	switch {
	case err == nil:
		if err := r.recordExternal(s, known.UUID, known.Email, now, creds.mtime); err != nil {
			return err
		}
		r.adoptKnown(s, known, raw, now, creds.mtime)
		return nil
	case !errors.Is(err, errUnknownLogin):
		return err
	}
	// Новый вход: чей он, говорит только API
	if !now.Before(o.expires()) {
		// Токен истёк, Claude Code обновит его сам — сверка после
		return nil
	}
	if now.Before(s.RetryAt) && !force {
		return nil
	}
	prof, err := r.Client.Profile(ctx, o.AccessToken)
	if err != nil {
		s.RetryAt = now.Add(retryDelay)
		return err
	}
	if err := r.recordExternal(s, prof.UUID, prof.Email, now, creds.mtime); err != nil {
		return err
	}
	a := s.byUUID(prof.UUID)
	if a == nil {
		s.Accounts = append(s.Accounts, Account{UUID: prof.UUID})
		a = &s.Accounts[len(s.Accounts)-1]
	}
	a.Email, a.OrgUUID, a.OAuth, a.SavedAt, a.Dead = prof.Email, prof.OrgUUID, raw, now, ""
	// Новый вход: неделя учётки спрашивается сразу
	a.UsageCheckedAt = time.Time{}
	r.copyOAuthAccount(a)
	if len(a.OAuthAccount) == 0 {
		minimal, err := json.Marshal(map[string]string{
			"accountUuid":      prof.UUID,
			"emailAddress":     prof.Email,
			"organizationUuid": prof.OrgUUID,
		})
		if err != nil {
			return fmt.Errorf("cannot encode oauthAccount: %w", err)
		}
		a.OAuthAccount = minimal
	}
	s.Active, s.SyncedMtime, s.RetryAt = a.UUID, creds.mtime, time.Time{}
	r.Log.Info("accounts: login saved", "account", a.Email)
	return nil
}

// Время внешнего перехода неизвестно: at — только момент обнаружения.
func (r *Rotator) recordExternal(s *Store, uuid, email string, now, mtime time.Time) error {
	if s.Active == uuid {
		return nil
	}
	rec := switchRecord{At: now, From: s.ActiveEmail(), To: email, Reason: "внешняя смена входа", Source: "external", TimeUncertain: true}
	if !mtime.IsZero() && !mtime.After(now) {
		rec.NotAfter = &mtime
		if s.Active != "" && !s.SyncedMtime.IsZero() && !s.SyncedMtime.After(mtime) {
			rec.NotBefore = &s.SyncedMtime
		}
	}
	return appendSwitch(r.Paths, rec)
}

// adoptKnown делает знакомый хранилищу вход активным: токены — в хранилище
func (r *Rotator) adoptKnown(s *Store, known *Account, raw json.RawMessage, now, mtime time.Time) {
	if known.UUID != s.Active {
		// /login на другую учётку: её неделя спрашивается сразу
		known.UsageCheckedAt = time.Time{}
	}
	known.OAuth, known.SavedAt, known.Dead = raw, now, ""
	s.Active, s.SyncedMtime = known.UUID, mtime
	r.copyOAuthAccount(known)
}

// copyOAuthAccount берёт oauthAccount из ~/.claude.json, если он этой учётки
func (r *Rotator) copyOAuthAccount(a *Account) {
	value, ref, err := readOAuthAccount(r.Paths.GlobalConfig)
	if err != nil {
		r.Log.Warn("accounts: oauthAccount not read", "error", err)
		return
	}
	if ref.AccountUUID == a.UUID && len(value) > 0 {
		a.OAuthAccount = value
	}
}

// fresh возвращает токены неактивной учётки, живущие ещё margin, обновляя
// их при нужде. Обновлённые токены сразу пишутся в хранилище: старый токен
// обновления после ответа сервера уже не годится
func (r *Rotator) fresh(ctx context.Context, s *Store, a *Account, margin time.Duration) (oauth, error) {
	if a.UUID == s.Active {
		return oauth{}, errors.New("the active account is refreshed by Claude Code, not by rotation")
	}
	now := r.Now()
	o, err := a.oauth()
	if err != nil {
		return oauth{}, err
	}
	if o.expires().After(now.Add(margin)) {
		return o, nil
	}
	got, err := r.Client.Refresh(ctx, o, now)
	if errors.Is(err, errDeadLogin) {
		a.Dead = "обновление отклонено (invalid_grant), нужен /login"
		r.Log.Warn("accounts: inactive login refresh failed", "account", a.Email, "error", err)
		return oauth{}, err
	}
	if err != nil {
		r.Log.Warn("accounts: inactive login refresh failed", "account", a.Email, "error", err)
		return oauth{}, err
	}
	if got.AccountUUID != "" && got.AccountUUID != a.UUID {
		// Токен обновления оказался чужим: сохранять ответ некуда
		a.Dead = "токен обновления выдан другой учётке, нужен /login"
		err := fmt.Errorf("refresh answered for another account")
		r.Log.Warn("accounts: inactive login refresh failed", "account", a.Email, "error", err)
		return oauth{}, err
	}
	updated, err := applyRefreshed(a.OAuth, got)
	if err != nil {
		r.Log.Warn("accounts: inactive login refresh failed", "account", a.Email, "error", err)
		return oauth{}, err
	}
	a.OAuth, a.SavedAt = updated, now
	if err := saveStore(r.Paths, s); err != nil {
		err = fmt.Errorf("refreshed tokens not saved: %w", err)
		r.Log.Warn("accounts: inactive login refresh failed", "account", a.Email, "error", err)
		return oauth{}, err
	}
	r.Log.Info("accounts: inactive login refreshed", "account", a.Email)
	return a.oauth()
}

// noSwitchReasons описывает причину, по которой каждая учётка не может
// стать целью текущего выбора.
func noSwitchReasons(s *Store, usage map[string]Usage, now time.Time) []string {
	unit := s.PlanUnit(now)
	reasons := make([]string, 0, len(s.Accounts))
	for i := range s.Accounts {
		a := &s.Accounts[i]
		label := a.Email
		if label == "" {
			label = a.UUID
		}
		st := inspectAccount(a, usage, now)
		switch st.reason {
		case accountDead:
			reasons = append(reasons, fmt.Sprintf("%s: мёртвая (%s)", label, st.detail))
		case accountBroken:
			reasons = append(reasons, fmt.Sprintf("%s: токены не разобраны (%s)", label, st.detail))
		case accountCleared:
			reasons = append(reasons, fmt.Sprintf("%s: вход очищен", label))
		case accountLoginExpiring:
			reasons = append(reasons, fmt.Sprintf("%s: вход истекает раньше чем через час (до %s)", label, st.loginEnds.Format(time.RFC3339)))
		case accountWeekFull:
			reasons = append(reasons, fmt.Sprintf("%s: неделя заполнена (%s)", label, unit.Percent(st.week.Used)))
		case accountAvailable:
			if a.UUID == s.Active {
				reasons = append(reasons, fmt.Sprintf("%s: остаётся активной по правилу выбора", label))
			} else {
				reasons = append(reasons, fmt.Sprintf("%s: неделя доступна (%s), сброс %s", label, unit.Percent(st.week.Used), ShortTime(st.resets)))
			}
		case accountWeekUnknown:
			reasons = append(reasons, fmt.Sprintf("%s: неделя неизвестна или не начата", label))
		}
	}
	if len(reasons) == 0 {
		return []string{"нет сохранённых учёток"}
	}
	return reasons
}

// fetchUsage спрашивает у API расход учётки и пишет его в снимок
func (r *Rotator) fetchUsage(ctx context.Context, s *Store, a *Account) error {
	now := r.Now()
	a.UsageCheckedAt = now
	o, ok, err := r.usageToken(ctx, s, a)
	if err != nil || !ok {
		return err
	}
	u, err := r.Client.Usage(ctx, o.AccessToken, now)
	if err != nil {
		return err
	}
	return r.Limits.Record(a.Email, u.Week, now)
}

// usageToken — токены для запроса расхода. Активную обновляет Claude Code,
// поэтому её токен берётся как есть, а истекающий — пропуск (false без
// ошибки): замер повторится через usageStale
func (r *Rotator) usageToken(ctx context.Context, s *Store, a *Account) (oauth, bool, error) {
	if a.UUID != s.Active {
		o, err := r.fresh(ctx, s, a, usageMargin)
		return o, err == nil, err
	}
	o, err := a.oauth()
	if err != nil {
		return o, false, err
	}
	if !o.expires().After(r.Now().Add(usageMargin)) {
		r.Log.Info("accounts: active login token expiring, usage not fetched", "account", a.Email)
		return o, false, nil
	}
	return o, true, nil
}

// Switch переключает на учётку вручную и выключает автоматическое
// переключение: иначе оно вернуло бы свой выбор через пару минут
func (r *Rotator) Switch(ctx context.Context, name string) (string, error) {
	if err := os.MkdirAll(r.Paths.StateDir, 0o755); err != nil {
		return "", fmt.Errorf("cannot create the state directory: %w", err)
	}
	unlock, _, err := flock(r.Paths.storeLock(), true)
	if err != nil {
		return "", err
	}
	defer unlock()
	s, err := loadStore(r.Paths)
	if err != nil {
		return "", err
	}
	if err := r.sync(ctx, s, true); err != nil {
		return "", fmt.Errorf("credentials not synced, no switch: %w", err)
	}
	target, err := s.find(name)
	if err != nil {
		return "", err
	}
	s.AutoOff = true
	if target.UUID != s.Active {
		if err := r.switchTo(ctx, s, target, "вручную", "manual"); err != nil {
			if saveErr := saveStore(r.Paths, s); saveErr != nil {
				r.Log.Error("accounts: store not saved", "error", saveErr)
			}
			return "", err
		}
	}
	return target.Email, saveStore(r.Paths, s)
}

var errJournalAfterSwitch = errors.New("switched, journal not written")

// switchTo кладёт токены учётки в файл Claude Code. Под блокировками
// обновления токена: сессия, обновляющая токен прежней учётки, либо
// закончит до подмены (и её новый токен уйдёт в хранилище), либо начнёт
// после и увидит уже новую учётку
func (r *Rotator) switchTo(ctx context.Context, s *Store, target *Account, reason, source string) error {
	if !eligible(target, r.Now()) {
		return fmt.Errorf("login of %s is not usable: %s", target.Email, target.Dead)
	}
	if _, err := r.fresh(ctx, s, target, switchMargin); err != nil {
		return err
	}
	release, err := claudeLocks(r.Paths)
	if err != nil {
		return err
	}
	creds, err := readCredentials(r.Paths.Credentials())
	if err != nil {
		release()
		return err
	}
	from := s.byUUID(s.Active)
	if len(creds.oauthRaw()) == 0 && s.Active != "" {
		release()
		s.SyncedMtime = time.Time{}
		return errors.New("credentials login removed, switch postponed until it is synced")
	}
	if raw := creds.oauthRaw(); len(raw) > 0 {
		cur, err := parseOAuth(raw)
		if err != nil {
			release()
			return err
		}
		if cur.cleared() {
			if from != nil && from.Dead == "" {
				from.Dead = "Claude Code стёр токены: обновление отклонено, нужен /login"
			}
		} else {
			owner, err := s.byLogin(cur)
			if err != nil && !errors.Is(err, errUnknownLogin) {
				release()
				return err
			}
			if owner == nil {
				// В файле вход, которого хранилище не знает (свежий /login):
				// подмена потеряла бы его — сначала сверка
				release()
				s.SyncedMtime = time.Time{}
				return errors.New("credentials hold an unknown login, switch postponed until it is saved")
			}
			if owner.UUID != s.Active {
				release()
				s.SyncedMtime = time.Time{}
				return errors.New("credentials changed account, switch postponed until it is synced")
			}
			owner.OAuth, owner.SavedAt = raw, r.Now()
			from = owner
		}
	}
	mtime, err := writeCredentials(r.Paths.Credentials(), creds, target.OAuth)
	release()
	if err != nil {
		return err
	}
	fromEmail := ""
	if from != nil {
		fromEmail = from.Email
	}
	s.Active, s.SyncedMtime, s.LastSwitch = target.UUID, mtime, r.Now()
	if len(target.OAuthAccount) > 0 {
		if err := replaceOAuthAccount(r.Paths, target.OAuthAccount); err != nil {
			// Токены уже переключены: oauthAccount — подпись учётки в /status
			r.Log.Warn("accounts: oauthAccount not replaced", "to", target.Email, "error", err)
		}
	}
	if err := appendSwitch(r.Paths, switchRecord{At: r.Now().Truncate(time.Second), From: fromEmail, To: target.Email, Reason: reason, Source: source}); err != nil {
		r.Log.Error("accounts: switched, journal not written", "from", fromEmail, "to", target.Email, "error", err)
		return fmt.Errorf("%w: from %s to %s: %w", errJournalAfterSwitch, fromEmail, target.Email, err)
	}
	r.Log.Info("accounts: switched", "from", fromEmail, "to", target.Email, "reason", reason)
	return nil
}

// switchRecord — строка журнала переключений
type switchRecord struct {
	At            time.Time  `json:"at"`
	From          string     `json:"from,omitempty"`
	To            string     `json:"to"`
	Reason        string     `json:"reason"`
	Source        string     `json:"source,omitempty"`
	TimeUncertain bool       `json:"timeUncertain,omitempty"`
	NotBefore     *time.Time `json:"notBefore,omitempty"`
	NotAfter      *time.Time `json:"notAfter,omitempty"`
}

func appendSwitch(p Paths, rec switchRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("cannot encode the switch record: %w", err)
	}
	f, err := os.OpenFile(p.Journal(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("cannot open the switch journal: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("cannot write the switch journal: %w", err)
	}
	return f.Close()
}

// SetAuto включает и выключает автоматическое переключение
func SetAuto(p Paths, on bool) error {
	if err := os.MkdirAll(p.StateDir, 0o755); err != nil {
		return fmt.Errorf("cannot create the state directory: %w", err)
	}
	unlock, _, err := flock(p.storeLock(), true)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := loadStore(p)
	if err != nil {
		return err
	}
	s.AutoOff = !on
	return saveStore(p, s)
}

// Forget удаляет учётку из хранилища
func Forget(p Paths, name string) (string, error) {
	unlock, _, err := flock(p.storeLock(), true)
	if err != nil {
		return "", err
	}
	defer unlock()
	s, err := loadStore(p)
	if err != nil {
		return "", err
	}
	a, err := s.find(name)
	if err != nil {
		return "", err
	}
	email := a.Email
	if a.UUID == s.Active {
		s.Active = ""
	}
	uuid := a.UUID
	kept := s.Accounts[:0]
	for i := range s.Accounts {
		if s.Accounts[i].UUID != uuid {
			kept = append(kept, s.Accounts[i])
		}
	}
	s.Accounts = kept
	return email, saveStore(p, s)
}

// MaybeRotate решает по снимку, есть ли ротации работа, и запускает её
// фоновым процессом. Строка статуса не ждёт ни сети, ни блокировок Claude
// Code, а прерванная таймаутом строки статуса ротация потеряла бы токен.
// executable — свой бинарь, args — подкоманда ротации
func MaybeRotate(p Paths, s *Store, usage map[string]Usage, now time.Time, executable string, request hookctx.Context, args ...string) (string, error) {
	home := request.Getenv("HOME")
	if home != "" {
		home = request.Abs(home)
	}
	why, err := pendingWithUnit(p, s, usage, now, s.PlanUnitForHome(now, home))
	if err != nil || why == "" {
		return "", err
	}
	if info, err := os.Stat(p.spawnMark()); err == nil && now.Sub(info.ModTime()) < spawnInterval {
		return "", nil
	}
	if err := os.MkdirAll(p.StateDir, 0o755); err != nil {
		return "", fmt.Errorf("cannot create the state directory: %w", err)
	}
	if err := os.WriteFile(p.spawnMark(), nil, 0o600); err != nil {
		return "", fmt.Errorf("cannot mark the rotation start: %w", err)
	}
	if err := os.Chtimes(p.spawnMark(), now, now); err != nil {
		return "", fmt.Errorf("cannot mark the rotation start: %w", err)
	}
	cmd := exec.Command(executable, args...)
	cmd.Env = append([]string{}, request.Env...)
	cmd.Dir = request.Cwd
	if err := desktop.StartDetached(cmd); err != nil {
		return "", fmt.Errorf("rotation not started: %w", err)
	}
	return why, nil
}

// pending — причина запустить ротацию или пусто
func pending(p Paths, s *Store, usage map[string]Usage, now time.Time) (string, error) {
	return pendingWithUnit(p, s, usage, now, s.PlanUnit(now))
}

func pendingWithUnit(p Paths, s *Store, usage map[string]Usage, now time.Time, unit Unit) (string, error) {
	if now.Before(s.RetryAt) {
		return "", nil
	}
	info, err := os.Stat(p.Credentials())
	if errors.Is(err, fs.ErrNotExist) {
		// Входа нет (ключ API или выход): ротации нечего делать
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("cannot stat Claude Code credentials: %w", err)
	}
	if !info.ModTime().Equal(s.SyncedMtime) {
		if now.Sub(info.ModTime()) >= settle {
			return "файл токенов изменился", nil
		}
		return "", nil
	}
	if len(s.Accounts) < 2 {
		return "", nil
	}
	if len(needsUsage(s, usage, now)) > 0 {
		return "неделя учётки неизвестна или пуста", nil
	}
	if s.AutoOff {
		return "", nil
	}
	activeAlive := false
	if active := s.byUUID(s.Active); active != nil {
		activeAlive = eligible(active, now)
	}
	if activeAlive && now.Sub(s.LastSwitch) < switchCooldown {
		return "", nil
	}
	if result := chooseResultWithUnit(s, usage, now, unit); result.account != nil && result.account.UUID != s.Active {
		return result.reason, nil
	}
	return "", nil
}

// Load читает хранилище для строки статуса и сводок
func Load(p Paths) (*Store, error) { return loadStore(p) }
