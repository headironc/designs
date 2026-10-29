package lifecycle

import (
	"context"
	"os"
	"reflect"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/headironc/designs/runner/internal/provider"
)

// This composition only observes the existing credential window. It does not
// invoke its rotation, installation, StopUnit, StartUnit or Login paths.
func admitObserver(ctx context.Context, p observerPlan, s *serviceFiles, consume func(observerAdmission) error) error {
	if os.Getuid() != 0 || os.Geteuid() != 0 || s == nil || s.files.uid != 0 || s.files.boundary != "/" || s.path != sessionParent+"/github-observer" || s.lockPath != sessionParent+"/session.lock" || !s.unchanged() {
		return errRefused
	}
	source, err := s.sibling("session")
	if err != nil {
		return errRefused
	}
	credential, err := readManagedGARM(source, p.SessionProducer, time.Now())
	if err != nil {
		source.close()
		return errRefused
	}
	defer credential.close()
	if credential.controller != p.Controller || bytesSHA(source.raw["request.json"]) != p.SessionRequest || credential.expires < p.RequiredUntil {
		return errRefused
	}
	sp, err := decodeSessionPlan(source.raw["request.json"])
	if err != nil {
		return errRefused
	}
	var issued sessionIntent
	if strictObject(source.raw["intent.json"], &issued) != nil {
		return errRefused
	}
	window, err := s.sibling("window")
	if err != nil {
		return errRefused
	}
	defer window.close()
	if exists, err := window.existing(); err != nil || !exists {
		return errRefused
	}
	var ready sessionWindowReady
	if _, err := (sessionAcquirer{files: s.files}).read(window, "window.ready.json", &ready); err != nil {
		return errRefused
	}
	producer, err := runningDigest()
	if err != nil {
		return errRefused
	}
	history := sessionHistory{prepare: prepareReader{files: s.files, parent: "/var/lib/nucleus-halo-first-connection-inputs/" + prepareOperation, release: "/var/lib/nucleus-halo-rebind-code/" + prepareReleaseSHA, releaseSHA: prepareReleaseSHA, planSHA: preparePlanSHA}, legacyDir: "/home/headiron/runner-deployment/halo-20261026/controller-start-verification", legacySHA: sessionLegacyOperationSHA}
	watchdog := sessionWatchdog{files: s.files, parent: pauseParent, policy: "/etc/nucleus-halo/provider-policy.json"}
	return history.withAdmissionHistory(ctx, producer, sp, time.Now, func(_ string, historyCheck func() error) error {
		return watchdog.withIdleState(ctx, p.Controller, func(_ string, watchdogCheck func() error) error {
			return provider.WithIdleController(ctx, p.Controller, func(providerCheck func() error, _ func(func() error) error) error {
				b, err := connectSessionBus(ctx)
				if err != nil {
					return errRefused
				}
				defer b.close()
				config, raw, err := pinDatabaseConfiguration(s.files, controllerConfig, b.gid)
				if err != nil {
					return errRefused
				}
				defer config.Close()
				defer clear(raw)
				if bytesSHA(raw) != issued.Origin.ConfigSHA {
					return errRefused
				}
				var configuration struct {
					Database struct {
						Passphrase string `toml:"passphrase"`
					} `toml:"database"`
				}
				if _, err := toml.Decode(string(raw), &configuration); err != nil || len(configuration.Database.Passphrase) != 32 {
					return errRefused
				}
				database, err := observeObserverDatabase(ctx, p, sp)
				if err != nil {
					return errRefused
				}
				defer clear(database.Payload)
				key, err := observerKey(p, database.Payload, []byte(configuration.Database.Passphrase))
				if err != nil {
					return errRefused
				}
				processes := sessionProcesses{proc: "/proc", realFS: true, serviceUID: b.uid}
				initialGateway, err := b.gatewayIdle(ctx)
				if err != nil {
					return errRefused
				}
				initialMachine, err := observeSessionMachine(ctx, "/var/snap/lxd/common/lxd/unix.socket", p.Controller)
				if err != nil {
					return errRefused
				}
				check := func(c context.Context) error {
					if c.Err() != nil || time.Now().Unix() >= credential.expires || !source.unchanged() || !window.unchanged() || !samePath(config, controllerConfig) || historyCheck() != nil || watchdogCheck() != nil || providerCheck() != nil || b.retirementTarget(c, sp, issued, ready) != nil || processes.check(c, sp.Boot, ready.After.Main) != nil {
						return errRefused
					}
					gateway, err := b.gatewayIdle(c)
					if err != nil || gateway != initialGateway {
						return errRefused
					}
					machine, err := observeSessionMachine(c, "/var/snap/lxd/common/lxd/unix.socket", p.Controller)
					if err != nil || machine != initialMachine {
						return errRefused
					}
					current, err := observeObserverDatabase(c, p, sp)
					defer clear(current.Payload)
					if err != nil || !reflect.DeepEqual(current, database) {
						return errRefused
					}
					client := newBoundSessionClient(nil, nil, func(c context.Context) error { return b.retirementTarget(c, sp, issued, ready) })
					defer client.close()
					if client.controller(c, credential.token, p.Controller) != nil || !source.unchanged() || !s.unchanged() || historyCheck() != nil || watchdogCheck() != nil || providerCheck() != nil {
						return errRefused
					}
					return nil
				}
				if check(ctx) != nil {
					return errRefused
				}
				return consume(observerAdmission{key: key, origin: observerOrigin{bytesSHA(source.raw["complete.json"]), database.Identity, issued.Origin.ConfigSHA, issued.Origin.Invocation, credential.expires}, check: check})
			})
		})
	})
}
