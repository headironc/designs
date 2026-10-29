package lifecycle

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
)

type windowAdmission struct {
	window       sessionWindow
	plan         sessionPlan
	ready        sessionWindowReady
	user         sessionDatabase
	passwordPath string
	closed       sync.Once
}

func (a *windowAdmission) origin() sessionOrigin {
	return sessionOrigin{WindowSHA: bytesSHA(a.window.store.raw["window.ready.json"]), Invocation: a.ready.After.Invocation, ConfigSHA: a.ready.ConfigSHA, User: a.user.User, Generation: a.user.Generation}
}

func (a *windowAdmission) close() {
	a.closed.Do(func() {
		a.window.services.close()
		a.window.store.close()
	})
}

func (a *windowAdmission) password() (string, error) {
	raw, err := a.window.store.files.read(a.passwordPath, 0600, 64*1024)
	password := strings.TrimSpace(string(raw))
	if err != nil || password == "" {
		return "", errRefused
	}
	return password, nil
}

func (a *windowAdmission) check(ctx context.Context) error {
	w, s := a.window, a.window.store
	if ctx.Err() != nil || w.now == nil || w.now().Unix() >= a.plan.Expires || !s.unchanged() || a.user.Controller != a.plan.Controller {
		return errRefused
	}
	reader := sessionAcquirer{files: s.files}
	var intent sessionWindowIntent
	if _, err := reader.read(s, "window.intent.json", &intent); err != nil {
		return errRefused
	}
	if !a.origin().valid(a.plan) || reader.verifyWindow(s, a.plan, a.origin(), intent.RequestSHA, w.producer) != nil {
		return errRefused
	}
	identity := func() error {
		boot, manager, err := w.services.identity(ctx)
		if err != nil || boot != a.plan.Boot || manager != intent.Manager {
			return errRefused
		}
		return nil
	}
	units := func() error {
		watchdog, err := w.services.watchdog(ctx, &intent.Watchdog)
		if err != nil || !pauseDrainedShape(watchdog) || pauseCompatible(intent.Watchdog, watchdog) != nil || w.services.watchdogDrained(ctx, intent.Watchdog, watchdog) != nil {
			return errRefused
		}
		controller, err := w.services.controller(ctx, nil)
		if err != nil || !equalSessionController(controller, a.ready.After) {
			return errRefused
		}
		listener, err := w.services.listener(ctx, controller.Main)
		if err != nil || listener != a.ready.Listener {
			return errRefused
		}
		return identity()
	}
	if identity() != nil || units() != nil {
		return errRefused
	}
	admission, err := w.admission(ctx)
	if err != nil || admission != intent.AdmissionSHA {
		return errRefused
	}
	installer := w.installer
	installer.store = s
	// A completed exchange is read back; apply cannot send a service job.
	if installer.apply(ctx) != nil || units() != nil || !s.unchanged() {
		return errRefused
	}
	return nil
}

func (w sessionWindow) loginAdmission(p sessionPlan, ready sessionWindowReady, user sessionDatabase) (*windowAdmission, error) {
	s := w.store
	reader := sessionAcquirer{files: s.files}
	var saved sessionWindowReady
	if _, err := reader.read(s, "window.ready.json", &saved); err != nil || !equalWindowReady(ready, saved) || !uuidPattern.MatchString(user.User) || user.Generation < 0 || user.Controller != p.Controller {
		return nil, errRefused
	}
	return &windowAdmission{window: w, plan: p, ready: ready, user: user, passwordPath: filepath.Join("/etc/nucleus-halo", "admin-password")}, nil
}
