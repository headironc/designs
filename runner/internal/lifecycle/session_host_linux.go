package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"reflect"
	"time"

	"github.com/headironc/designs/runner/internal/provider"
)

// The gateway remains stopped throughout this window, including while the
// controller is stopped. Observe loaded upstream contracts without requiring
// those upstreams to be active or sending another gateway stop request.
func (b *sessionBus) gatewayIdle(ctx context.Context) (string, error) {
	host := b.host
	host.nginx = "/usr/sbin/nginx"
	g := gatewayBus{unitBus: b.unitBus, host: host}
	u, err := b.api.GetUnitPropertiesContext(ctx, gatewayUnit)
	if err != nil || g.loadedUpstreams(ctx, u, false) != nil {
		return "", errRefused
	}
	before, err := g.rawSnapshot(ctx, false)
	if err != nil || !gatewayDrained(before) || host.enrich(&before, nil) != nil {
		return "", errRefused
	}
	after, err := g.snapshot(ctx, &before)
	if err != nil || !gatewayDrained(after) || !equalSessionController(before, after) {
		return "", errRefused
	}
	dev, ino, empty, err := serviceGroup(host.cgroup, gatewayGroup, host.realFS)
	if err != nil || !empty || dev != after.GroupDevice || ino != after.GroupInode {
		return "", errRefused
	}
	raw, err := json.Marshal(after)
	if err != nil {
		return "", errRefused
	}
	return bytesSHA(raw), nil
}

// This is the production composition, not an alternative issuer. All locks
// live in the synchronous admission call until the one Login and publication
// finish. Only the original StartUnit job can yield the provider lock.
func admitSessionWindow(ctx context.Context, p sessionPlan, request []byte, session *serviceFiles, consume func(sessionAdmission) error) error {
	if os.Getuid() != 0 || os.Geteuid() != 0 || session == nil || consume == nil || session.files.uid != 0 || session.files.boundary != "/" || session.path != sessionParent+"/session" || session.lockPath != sessionParent+"/session.lock" || !session.unchanged() {
		return errRefused
	}
	decoded, err := decodeSessionPlan(request)
	if err != nil || decoded != p {
		return errRefused
	}
	producer, err := runningDigest()
	if err != nil {
		return errRefused
	}
	files := protectedFiles{uid: 0, boundary: "/"}
	history := sessionHistory{
		prepare:   prepareReader{files: files, parent: "/var/lib/nucleus-halo-first-connection-inputs/" + prepareOperation, release: "/var/lib/nucleus-halo-rebind-code/" + prepareReleaseSHA, releaseSHA: prepareReleaseSHA, planSHA: preparePlanSHA},
		legacyDir: "/home/headiron/runner-deployment/halo-20261026/controller-start-verification", legacySHA: sessionLegacyOperationSHA,
	}
	watchdog := sessionWatchdog{files: files, parent: pauseParent, policy: "/etc/nucleus-halo/provider-policy.json"}
	return history.withAdmissionHistory(ctx, producer, p, time.Now, func(historySHA string, historyCheck func() error) error {
		return watchdog.withIdleState(ctx, p.Controller, func(watchdogSHA string, watchdogCheck func() error) error {
			return provider.WithIdleController(ctx, p.Controller, func(providerCheck func() error, startLease func(func() error) error) error {
				b, err := connectSessionBus(ctx)
				if err != nil {
					return errRefused
				}
				window, err := session.sibling("window")
				if err != nil {
					b.close()
					return errRefused
				}
				transferred := false
				defer func() {
					if !transferred {
						b.close()
						window.close()
					}
				}()
				user, err := observeSessionDatabase(ctx, p)
				if err != nil {
					return errRefused
				}
				processes := sessionProcesses{proc: "/proc", realFS: true, serviceUID: b.uid}
				admission := func(c context.Context) (string, error) {
					if c.Err() != nil || historyCheck() != nil || watchdogCheck() != nil || providerCheck() != nil {
						return "", errRefused
					}
					current, err := b.controller(c, nil)
					if err != nil || processes.check(c, p.Boot, current.Main) != nil {
						return "", errRefused
					}
					gateway, err := b.gatewayIdle(c)
					if err != nil {
						return "", errRefused
					}
					database, err := observeSessionDatabase(c, p)
					if err != nil || !reflect.DeepEqual(database, user) {
						return "", errRefused
					}
					machine, err := observeSessionMachine(c, "/var/snap/lxd/common/lxd/unix.socket", p.Controller)
					if err != nil || historyCheck() != nil || watchdogCheck() != nil || providerCheck() != nil || processes.check(c, p.Boot, current.Main) != nil {
						return "", errRefused
					}
					raw, err := json.Marshal(map[string]string{"history": historySHA, "watchdog": watchdogSHA, "database": database.Identity, "machine": machine, "gateway": gateway})
					if err != nil {
						return "", errRefused
					}
					return bytesSHA(raw), nil
				}
				domain := sessionDomain{store: window, plan: p, requestSHA: bytesSHA(request), user: user, now: time.Now, services: b}
				w := sessionWindow{store: window, services: b, installer: sessionInstaller{store: window, path: controllerConfig, gid: b.gid}, producer: producer, now: time.Now, entropy: rand.Reader, startLease: startLease, admission: admission, probe: domain.prepare, domain: domain.verify}
				ready, err := w.execute(ctx, p, request)
				if err != nil {
					return errRefused
				}
				a, err := w.loginAdmission(p, ready, user)
				if err != nil {
					return errRefused
				}
				transferred = true
				defer a.close()
				return consume(a)
			})
		})
	})
}
