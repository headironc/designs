package lifecycle

import (
	"context"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	lxd "github.com/canonical/lxd/client"
)

func connectionImage(ctx context.Context, socket string, p connectionPlan) error {
	return withMachineClient(ctx, socket, func(c lxd.InstanceServer) error {
		image, _, err := c.GetImage(p.ImageSHA)
		if err != nil || image == nil || image.Fingerprint != p.ImageSHA || image.Type != "virtual-machine" || image.Public {
			return errRefused
		}
		if p.Scale.Image != p.ImageSHA {
			alias, _, err := c.GetImageAlias(p.Scale.Image)
			if err != nil || alias == nil || alias.Name != p.Scale.Image || alias.Target != p.ImageSHA || alias.Type != "virtual-machine" {
				return errRefused
			}
		}
		return nil
	})
}

func admitConnection(webhookPath string) func(context.Context, connectionPlan, *serviceFiles, func(connectionAdmission) error) error {
	return func(ctx context.Context, p connectionPlan, s *serviceFiles, consume func(connectionAdmission) error) error {
		if s == nil || s.path != sessionParent+"/connection" {
			return errRefused
		}
		return withAppSource(ctx, p.Source, s, func(app appAdmission) error {
			boot, manager, err := app.services.identity(ctx)
			if err != nil || boot != app.session.Boot || managerOwner(manager) == "" {
				return errRefused
			}
			check := func(c context.Context) error {
				if _, err := app.check(c); err != nil {
					return errRefused
				}
				config, raw, err := pinDatabaseConfiguration(s.files, controllerConfig, app.services.gid)
				if err != nil {
					return errRefused
				}
				defer config.Close()
				defer clear(raw)
				var native struct {
					Default struct {
						Webhooks bool `toml:"enable_webhook_management"`
					} `toml:"default"`
				}
				if _, err := toml.Decode(string(raw), &native); err != nil || native.Default.Webhooks || bytesSHA(raw) != app.origin.Config {
					return errRefused
				}
				if connectionImage(c, "/var/snap/lxd/common/lxd/unix.socket", p) != nil {
					return errRefused
				}
				currentBoot, currentManager, err := app.services.identity(c)
				if err != nil || currentBoot != boot || currentManager != manager {
					return errRefused
				}
				return nil
			}
			webhook := func() (string, error) {
				if p.RepositoryMode != "create" {
					return "", errRefused
				}
				raw, err := s.files.read(webhookPath, 0600, 8192)
				defer clear(raw)
				if err != nil || len(raw) < 32 || bytesSHA(raw) != p.WebhookSHA || !validToken(string(raw)) || strings.TrimSpace(string(raw)) != string(raw) {
					return "", errRefused
				}
				return string(raw), nil
			}
			if check(ctx) != nil {
				return errRefused
			}
			return consume(connectionAdmission{app: app, control: app.services, boot: boot, manager: manager, controller: app.ready.After, check: check, webhook: webhook})
		})
	}
}

func recoverConnectionStop(ctx context.Context, s *serviceFiles, p connectionPlan, original connectionIntent, step string) error {
	// Original recovery does not need a live GARM bearer or recreate an App
	// signature. It remains bound to the original manager and service identity.
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	b, err := connectSessionBus(ctx)
	if err != nil {
		return errRefused
	}
	defer b.close()
	return stopConnectionController(ctx, s, p, original, step, b, time.Now)
}
